package dispatch

import (
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/logger"
	"sync"
	"time"
)

// maxAllowedTime is the maximum time a plugin is allowed to process a packet.
// If this time is exceeded. The warning is logged and the packet is passed
// and the session released on behalf of the offending plugin
const maxAllowedTime = 30 * time.Second

// NfDrop is NF_DROP constant
const NfDrop = 0

// NfAccept is the NF_ACCEPT constant
const NfAccept = 1

//NfqueueHandlerFunction defines a pointer to a nfqueue callback function
type NfqueueHandlerFunction func(NfqueueMessage, uint32, bool) NfqueueResult

// NfqueueResult returns status and other information from a subscription handler function
type NfqueueResult struct {
	Owner          string
	PacketMark     uint32
	SessionRelease bool
}

// NfqueueMessage is used to pass nfqueue traffic to interested plugins
type NfqueueMessage struct {
	Session        *SessionEntry
	MsgTuple       Tuple
	Packet         gopacket.Packet
	Length         int
	ClientToServer bool
	IP4Layer       *layers.IPv4
	IP6Layer       *layers.IPv6
	TCPLayer       *layers.TCP
	UDPLayer       *layers.UDP
	ICMPv4Layer    *layers.ICMPv4
	Payload        []byte
}

// nfqueueList holds the nfqueue subscribers
var nfqueueList map[string]SubscriptionHolder

// nfqueueListMutex is a lock for the nfqueueList
var nfqueueListMutex sync.Mutex

// InsertNfqueueSubscription adds a subscription for receiving nfqueue messages
func InsertNfqueueSubscription(owner string, priority int, function NfqueueHandlerFunction) {
	var holder SubscriptionHolder
	logger.Info("Adding NFQueue Event Subscription (%s, %d)\n", owner, priority)

	holder.Owner = owner
	holder.Priority = priority
	holder.NfqueueFunc = function
	nfqueueListMutex.Lock()
	nfqueueList[owner] = holder
	nfqueueListMutex.Unlock()
}

// AttachNfqueueSubscriptions attaches active nfqueue subscriptions to the argumented SessionEntry
func AttachNfqueueSubscriptions(session *SessionEntry) {
	session.subLocker.Lock()
	session.subscriptions = make(map[string]SubscriptionHolder)

	for index, element := range nfqueueList {
		session.subscriptions[index] = element
	}
	session.subLocker.Unlock()
}

// MirrorNfqueueSubscriptions creates a copy of the subscriptions for the argumented SessionEntry
func MirrorNfqueueSubscriptions(session *SessionEntry) map[string]SubscriptionHolder {
	mirror := make(map[string]SubscriptionHolder)
	session.subLocker.Lock()

	for k, v := range session.subscriptions {
		mirror[k] = v
	}

	session.subLocker.Unlock()
	return (mirror)
}

// ReleaseSession is called by a subscriber to stop receiving traffic for a session
func ReleaseSession(session *SessionEntry, owner string) {
	session.subLocker.Lock()
	defer session.subLocker.Unlock()
	origLen := len(session.subscriptions)
	if origLen == 0 {
		return
	}
	delete(session.subscriptions, owner)
	len := len(session.subscriptions)
	if origLen != len {
		logger.Debug("Removing %s session nfqueue subscription for session %d\n", owner, session.SessionID)
	}
	if len == 0 {
		logger.Debug("Zero subscribers reached - settings bypass_packetd=true for session %d\n", session.SessionID)
		dict.AddSessionEntry(session.ConntrackID, "bypass_packetd", true)
	}
}

// nfqueueCallback is the callback for the packet
// return the mark to set on the packet
func nfqueueCallback(ctid uint32, packet gopacket.Packet, packetLength int, pmark uint32) (int, uint32) {
	var mess NfqueueMessage
	//printSessionTable()

	mess.Packet = packet
	mess.Length = packetLength

	// get the IPv4 and IPv6 layers
	ip4Layer := mess.Packet.Layer(layers.LayerTypeIPv4)
	ip6Layer := mess.Packet.Layer(layers.LayerTypeIPv6)

	if ip4Layer != nil {
		mess.IP4Layer = ip4Layer.(*layers.IPv4)
		mess.MsgTuple.Protocol = uint8(mess.IP4Layer.Protocol)
		mess.MsgTuple.ClientAddress = dupIP(mess.IP4Layer.SrcIP)
		mess.MsgTuple.ServerAddress = dupIP(mess.IP4Layer.DstIP)
	} else if ip6Layer != nil {
		mess.IP6Layer = ip6Layer.(*layers.IPv6)
		mess.MsgTuple.Protocol = uint8(mess.IP6Layer.NextHeader) // FIXME - is this the correct field?
		mess.MsgTuple.ClientAddress = dupIP(mess.IP6Layer.SrcIP)
		mess.MsgTuple.ServerAddress = dupIP(mess.IP6Layer.DstIP)
	} else {
		return NfAccept, pmark
	}

	newSession := ((pmark & 0x10000000) != 0)

	// get the TCP layer
	tcpLayer := mess.Packet.Layer(layers.LayerTypeTCP)
	if tcpLayer != nil {
		mess.TCPLayer = tcpLayer.(*layers.TCP)
		mess.MsgTuple.ClientPort = uint16(mess.TCPLayer.SrcPort)
		mess.MsgTuple.ServerPort = uint16(mess.TCPLayer.DstPort)
	}

	// get the UDP layer
	udpLayer := mess.Packet.Layer(layers.LayerTypeUDP)
	if udpLayer != nil {
		mess.UDPLayer = udpLayer.(*layers.UDP)
		mess.MsgTuple.ClientPort = uint16(mess.UDPLayer.SrcPort)
		mess.MsgTuple.ServerPort = uint16(mess.UDPLayer.DstPort)
	}

	// get the ICMPv4 layer
	icmpLayerV4 := mess.Packet.Layer(layers.LayerTypeICMPv4)
	if icmpLayerV4 != nil {
		mess.ICMPv4Layer = icmpLayerV4.(*layers.ICMPv4)
		// For ICMP we set the ports to the ICMP ID
		// So we can use the standard tuple
		mess.MsgTuple.ClientPort = uint16(mess.ICMPv4Layer.Id)
		mess.MsgTuple.ServerPort = uint16(mess.ICMPv4Layer.Id)
	}

	// FIXME ICMPv6

	// get the Application layer
	appLayer := mess.Packet.ApplicationLayer()
	if appLayer != nil {
		mess.Payload = appLayer.Payload()
	}

	logger.Trace("nfqueue event[%d]: %v \n", ctid, mess.MsgTuple)

	session, clientToServer := lookupSessionEntry(mess, ctid)
	mess.Session = session
	mess.ClientToServer = clientToServer

	if session == nil {
		if !newSession {
			// If we did not find the session in the session table, and this isn't a new packet
			// Then we somehow missed the first packet - Just mark the connection as bypassed
			// and return the packet
			logger.Info("Ignoring mid-session packet: %s %d\n", mess.MsgTuple, ctid)
			dict.AddSessionEntry(ctid, "bypass_packetd", true)
			return NfAccept, pmark
		}
		session = createSessionEntry(mess, ctid)
		mess.Session = session
	} else {
		if newSession {
			// If this is a new session and a session was found, it may have just been an aborted session
			// (The first packet was dropped before conntrack confirm)
			// In this case, just drop the old session. However, if the old session was conntrack confirmed
			// something is not correct.
			if session.ConntrackConfirmed {
				logger.Err("Conflicting session tuple: %s  %d != %d\n", mess.MsgTuple, ctid, session.ConntrackID)
			} else {
				logger.Debug("Conflicting session tuple: %s  %d != %d\n", mess.MsgTuple, ctid, session.ConntrackID)
				removeSessionEntry(mess.MsgTuple.String())
				session = createSessionEntry(mess, ctid)
				mess.Session = session
			}
		}

		// Also check that the conntrack ID matches. Log an error if it does not
		if session.ConntrackID != ctid {
			logger.Err("Conntrack ID mismatch: %s  %d != %d %v\n", mess.MsgTuple, ctid, session.ConntrackID, session.ConntrackConfirmed)
		}
	}

	// Update some accounting bits
	session.LastActivityTime = time.Now()
	session.PacketCount++
	session.ByteCount += uint64(mess.Length)
	session.EventCount++

	return callSubscribers(ctid, session, mess, pmark, newSession)
}

// callSubscribers calls all the nfqueue message subscribers (plugins)
// and returns a verdict and the new mark
func callSubscribers(ctid uint32, session *SessionEntry, mess NfqueueMessage, pmark uint32, newSession bool) (int, uint32) {
	resultsChannel := make(chan NfqueueResult)

	// We loop and increment the priority until all subscriptions have been called
	sublist := MirrorNfqueueSubscriptions(session)
	subtotal := len(sublist)
	subcount := 0
	priority := 0
	var timeMap = make(map[string]float64)
	var timeMapLock = sync.RWMutex{}

	for subcount != subtotal {
		// Counts the total number of calls made for each priority so we know
		// how many NfqueueResult's to read from the result channel
		hitcount := 0

		// Call all of the subscribed handlers for the current priority
		for key, val := range sublist {
			if val.Priority != priority {
				continue
			}
			logger.Trace("Calling nfqueue  plugin:%s priority:%d session_id:%d\n", key, priority, session.SessionID)
			go func(key string, val SubscriptionHolder) {
				timeoutTimer := time.NewTimer(maxAllowedTime)
				c := make(chan NfqueueResult, 1)
				t1 := getMicroseconds()

				go func() { c <- val.NfqueueFunc(mess, ctid, newSession) }()

				select {
				case result := <-c:
					resultsChannel <- result
					timeoutTimer.Stop()
				case <-timeoutTimer.C:
					logger.Err("Timeout reached while processing nfqueue. plugin:%s\n", key)
					resultsChannel <- NfqueueResult{Owner: key, PacketMark: 0, SessionRelease: true}
				}

				timediff := (float64(getMicroseconds()-t1) / 1000.0)
				timeMapLock.Lock()
				timeMap[val.Owner] = timediff
				timeMapLock.Unlock()

				logger.Trace("Finished nfqueue plugin:%s PRI:%d SID:%d ms:%.1f\n", key, priority, session.SessionID, timediff)
			}(key, val)
			hitcount++
			subcount++
		}

		// Add the mark bits returned from each handler and remove the session
		// subscription for any that set the SessionRelease flag
		for i := 0; i < hitcount; i++ {
			select {
			case result := <-resultsChannel:
				pmark |= result.PacketMark
				if result.SessionRelease {
					ReleaseSession(session, result.Owner)
				}
			}
		}

		// Increment the priority and keep looping until we've called all subscribers
		priority++
		if priority > 100 {
			logger.Err("Priority > 100 Constraint failed! %d %d %d %v", subcount, subtotal, priority, sublist)
			panic("Constraint failed - infinite loop detected")
		}
	}

	if logger.IsLogEnabledSource(logger.LogLevelTrace, "dispatch_timer") {
		timeMapLock.RLock()
		logger.LogMessageSource(logger.LogLevelTrace, "dispatch_timer", "Timer Map: %v\n", timeMap)
		timeMapLock.RUnlock()
	}

	// return the updated mark to be set on the packet
	return NfAccept, pmark
}

// lookupSessionEntry looks up a session in the session table
// returns the session if found and a bool representing the direction
// true = forward, false = reverse
func lookupSessionEntry(mess NfqueueMessage, ctid uint32) (*SessionEntry, bool) {
	// use the packet tuple to find the session
	session, ok := findSessionEntry(mess.MsgTuple.String())
	if ok {
		logger.Trace("Session Found %d in table\n", session.SessionID)
		return session, true
	}

	// if we didn't find the session in the table look again with with the tuple in reverse
	session, ok = findSessionEntry(mess.MsgTuple.StringReverse())

	// If we already have a session entry update the existing, otherwise create a new entry for the table.
	if ok {
		logger.Trace("Session Found %d in table\n", session.SessionID)
		return session, false
	}

	return nil, true
}

// createSessionEntry creates a new session and inserts the forward mapping
// into the session table
func createSessionEntry(mess NfqueueMessage, ctid uint32) *SessionEntry {
	session := new(SessionEntry)
	session.SessionID = nextSessionID()
	session.ConntrackID = ctid
	session.CreationTime = time.Now()
	session.PacketCount = 1
	session.ByteCount = uint64(mess.Length)
	session.LastActivityTime = time.Now()
	session.ClientSideTuple = mess.MsgTuple
	session.EventCount = 1
	session.ConntrackConfirmed = false
	session.attachments = make(map[string]interface{})
	AttachNfqueueSubscriptions(session)
	logger.Trace("Session Adding %d to table\n", session.SessionID)
	insertSessionEntry(mess.MsgTuple.String(), session)
	return session
}

// getMicroseconds returns the current clock in microseconds
func getMicroseconds() int64 {
	return time.Now().UnixNano() / int64(time.Microsecond)
}
