package stats

import (
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/c9s/goprocinfo/linux"
	"github.com/untangle/packetd/services/dict"
	"github.com/untangle/packetd/services/dispatch"
	"github.com/untangle/packetd/services/logger"
	"github.com/untangle/packetd/services/reports"
	"github.com/untangle/packetd/services/settings"
)

const pluginName = "stats"
const interfaceStatLogIntervalSec = 10
const pingCheckIntervalSec = 5
const pingCheckTarget = "www.google.com"

var statsCollector [256]*Collector
var statsLocker [256]sync.Mutex

var interfaceInfoMap map[string]*interfaceDetail
var interfaceInfoLocker sync.RWMutex

var interfaceStatsMap map[string]*linux.NetworkStat
var interfaceChannel = make(chan bool)
var pingChannel = make(chan bool)

var randSrc rand.Source
var randGen *rand.Rand

type interfaceDetail struct {
	interfaceID int
	deviceName  string
	netAddress  string
	pingMode    int
	wanFlag     bool
}

// PluginStartup function is called to allow plugin specific initialization.
func PluginStartup() {
	logger.Info("PluginStartup(%s) has been called\n", pluginName)

	// we use random numbers in our active ping packets to help detect valid replies
	randSrc = rand.NewSource(time.Now().UnixNano())
	randGen = rand.New(randSrc)

	for x := 0; x < 256; x++ {
		statsCollector[x] = CreateCollector()
	}

	interfaceStatsMap = make(map[string]*linux.NetworkStat)
	interfaceInfoMap = make(map[string]*interfaceDetail)

	// FIXME - this is currently only loaded once during startup
	loadInterfaceInfoMap()

	go interfaceTask()
	go pingTask()

	dispatch.InsertNfqueueSubscription(pluginName, dispatch.StatsPriority, PluginNfqueueHandler)
}

// PluginShutdown function called when the daemon is shutting down.
func PluginShutdown() {
	logger.Info("PluginShutdown(%s) has been called\n", pluginName)

	interfaceChannel <- true

	select {
	case <-interfaceChannel:
		logger.Info("Successful shutdown of interfaceTask\n")
	case <-time.After(10 * time.Second):
		logger.Warn("Failed to properly shutdown interfaceTask\n")
	}

	pingChannel <- true

	select {
	case <-pingChannel:
		logger.Info("Successful shutdown of pingTask\n")
	case <-time.After(10 * time.Second):
		logger.Warn("Failed to properly shutdown pingTask\n")
	}

}

// PluginNfqueueHandler is called to handle nfqueue packet data.
func PluginNfqueueHandler(mess dispatch.NfqueueMessage, ctid uint32, newSession bool) dispatch.NfqueueResult {
	var result dispatch.NfqueueResult

	// we release by default unless logic below changes the flag
	result.SessionRelease = true

	// if this is a new session attach the current time
	if newSession {
		mess.Session.PutAttachment("stats_timer", time.Now())
		logHopCount(ctid, mess, "client_hops")
	}

	// ignore C2S packets but keep scanning until we get the first server response
	if mess.ClientToServer {
		result.SessionRelease = false
		return result
	}

	// get the hop count for the server
	logHopCount(ctid, mess, "server_hops")

	// We have a packet from the server so we calculate the latency as the
	// time elapsed since the first client packet was transmitted
	xmittime := mess.Session.GetAttachment("stats_timer")
	if xmittime == nil {
		logger.Warn("Missing stats_timer for session %d\n", ctid)
		return result
	}

	// We have a packet from the server so we calculate the latency as the
	// time elapsed sincethe first client packet was transmitted
	duration := time.Since(xmittime.(time.Time))
	interfaceID := mess.Session.GetServerInterfaceID()

	// ignore local traffic
	if interfaceID == 255 {
		return result
	}
	// log and ignore traffic to unknown interface
	if interfaceID == 0 {
		//the server interface is set in the conntrack new event
		logger.Warn("Unknown interface ID: %v\n", mess.Session.GetClientSideTuple())
		return result
	}

	statsLocker[interfaceID].Lock()
	statsCollector[interfaceID].AddDataPointLimited(float64(duration.Nanoseconds())/1000000.0, 2.0)
	logger.Debug("Logging latency sample: %d, %v, %v ms\n", interfaceID, mess.Session.GetServerSideTuple().ServerAddress, (duration.Nanoseconds() / 1000000))
	statsLocker[interfaceID].Unlock()

	return result
}

func interfaceTask() {

	for {
		select {
		case <-interfaceChannel:
			interfaceChannel <- true
			return
		case <-time.After(time.Second * time.Duration(interfaceStatLogIntervalSec)):
			logger.Debug("Collecting interface statistics\n")
			collectInterfaceStats(interfaceStatLogIntervalSec)

			for i := 0; i < 256; i++ {
				statsLocker[i].Lock()
				if statsCollector[i].Latency1Min.Value != 0.0 {
					statsCollector[i].dumpStatistics(i)
				}
				statsLocker[i].Unlock()
			}
		}
	}
}

// collectInterfaceStats gets the stats for every interface and then
// calculates and logs the difference since the last time it was called
func collectInterfaceStats(seconds uint64) {
	var statInfo *linux.NetworkStat
	var diffInfo linux.NetworkStat

	procData, err := linux.ReadNetworkStat("/proc/net/dev")
	if err != nil {
		logger.Err("Error reading interface statistics:%v\n", err)
		return
	}

	var istats []InterfaceStatsJSON
	for i := 0; i < len(procData); i++ {
		item := procData[i]

		// ignore loopback and dummy interfaces
		if item.Iface == "lo" ||
			strings.HasPrefix(item.Iface, "dummy") {
			continue
		}

		statInfo = interfaceStatsMap[item.Iface]

		if statInfo == nil {
			// if no entry for the interface use the existing values as the starting point
			statInfo = new(linux.NetworkStat)
			statInfo.Iface = item.Iface
			statInfo.RxBytes = item.RxBytes
			statInfo.RxPackets = item.RxPackets
			statInfo.RxErrs = item.RxErrs
			statInfo.RxDrop = item.RxDrop
			statInfo.RxFifo = item.RxFifo
			statInfo.RxFrame = item.RxFrame
			statInfo.RxCompressed = item.RxCompressed
			statInfo.RxMulticast = item.RxMulticast
			statInfo.TxBytes = item.TxBytes
			statInfo.TxPackets = item.TxPackets
			statInfo.TxErrs = item.TxErrs
			statInfo.TxDrop = item.TxDrop
			statInfo.TxFifo = item.TxFifo
			statInfo.TxColls = item.TxColls
			statInfo.TxCarrier = item.TxCarrier
			statInfo.TxCompressed = item.TxCompressed
			interfaceStatsMap[item.Iface] = statInfo
		} else {
			// found the interface entry so calculate the difference since last time
			// pass previous values as pointers so they can be updated after the calculation
			diffInfo.Iface = item.Iface
			diffInfo.RxBytes = calculateDifference(&statInfo.RxBytes, item.RxBytes)
			diffInfo.RxPackets = calculateDifference(&statInfo.RxPackets, item.RxPackets)
			diffInfo.RxErrs = calculateDifference(&statInfo.RxErrs, item.RxErrs)
			diffInfo.RxDrop = calculateDifference(&statInfo.RxDrop, item.RxDrop)
			diffInfo.RxFifo = calculateDifference(&statInfo.RxFifo, item.RxFifo)
			diffInfo.RxFrame = calculateDifference(&statInfo.RxFrame, item.RxFrame)
			diffInfo.RxCompressed = calculateDifference(&statInfo.RxCompressed, item.RxCompressed)
			diffInfo.RxMulticast = calculateDifference(&statInfo.RxMulticast, item.RxMulticast)
			diffInfo.TxBytes = calculateDifference(&statInfo.TxBytes, item.TxBytes)
			diffInfo.TxPackets = calculateDifference(&statInfo.TxPackets, item.TxPackets)
			diffInfo.TxErrs = calculateDifference(&statInfo.TxErrs, item.TxErrs)
			diffInfo.TxDrop = calculateDifference(&statInfo.TxDrop, item.TxDrop)
			diffInfo.TxFifo = calculateDifference(&statInfo.TxFifo, item.TxFifo)
			diffInfo.TxColls = calculateDifference(&statInfo.TxColls, item.TxColls)
			diffInfo.TxCarrier = calculateDifference(&statInfo.TxCarrier, item.TxCarrier)
			diffInfo.TxCompressed = calculateDifference(&statInfo.TxCompressed, item.TxCompressed)

			// Update current values
			interfaceStatsMap[item.Iface] = &item

			// convert the interface name to the ID value
			interfaceID := getInterfaceIDValue(diffInfo.Iface)

			// negative return means we don't know the ID so we set latency to zero
			// otherwise we get the total moving average
			if interfaceID < 0 {
				logger.Debug("Skipping unknown interface: %s\n", diffInfo.Iface)
			} else {
				statsLocker[interfaceID].Lock()
				c := statsCollector[interfaceID].MakeCopy()
				statsLocker[interfaceID].Unlock()

				istat := MakeInterfaceStatsJSON(interfaceID, c.Latency1Min.Value, c.Latency5Min.Value, c.Latency15Min.Value)
				istats = append(istats, istat)

				logInterfaceStats(seconds, interfaceID, c, &diffInfo)
			}
		}
	}

	allstats := MakeStatsJSON(istats)
	WriteStatsJSON(allstats)
}

func logInterfaceStats(seconds uint64, interfaceID int, collector Collector, diffInfo *linux.NetworkStat) {
	columns := map[string]interface{}{
		"time_stamp":         time.Now(),
		"interface_id":       interfaceID,
		"device_name":        diffInfo.Iface,
		"latency_1":          collector.Latency1Min.Value,
		"latency_5":          collector.Latency5Min.Value,
		"latency_15":         collector.Latency15Min.Value,
		"latency_variance":   collector.LatencyVariance.StdDeviation,
		"rx_bytes":           diffInfo.RxBytes,
		"rx_bytes_rate":      diffInfo.RxBytes / seconds,
		"rx_packets":         diffInfo.RxPackets,
		"rx_packets_rate":    diffInfo.RxPackets / seconds,
		"rx_errs":            diffInfo.RxErrs,
		"rx_errs_rate":       diffInfo.RxErrs / seconds,
		"rx_drop":            diffInfo.RxDrop,
		"rx_drop_rate":       diffInfo.RxDrop / seconds,
		"rx_fifo":            diffInfo.RxFifo,
		"rx_fifo_rate":       diffInfo.RxFifo / seconds,
		"rx_frame":           diffInfo.RxFrame,
		"rx_frame_rate":      diffInfo.RxFrame / seconds,
		"rx_compressed":      diffInfo.RxCompressed,
		"rx_compressed_rate": diffInfo.RxCompressed / seconds,
		"rx_multicast":       diffInfo.RxMulticast,
		"rx_multicast_rate":  diffInfo.RxMulticast / seconds,
		"tx_bytes":           diffInfo.TxBytes,
		"tx_bytes_rate":      diffInfo.TxBytes / seconds,
		"tx_packets":         diffInfo.TxPackets,
		"tx_packets_rate":    diffInfo.TxPackets / seconds,
		"tx_errs":            diffInfo.TxErrs,
		"tx_errs_rate":       diffInfo.TxErrs / seconds,
		"tx_drop":            diffInfo.TxDrop,
		"tx_drop_rate":       diffInfo.TxDrop / seconds,
		"tx_fifo":            diffInfo.TxFifo,
		"tx_fifo_rate":       diffInfo.TxFifo / seconds,
		"tx_colls":           diffInfo.TxColls,
		"tx_colls_rate":      diffInfo.TxColls / seconds,
		"tx_carrier":         diffInfo.TxCarrier,
		"tx_carrier_rate":    diffInfo.TxCarrier / seconds,
		"tx_compressed":      diffInfo.TxCompressed,
		"tx_compressed_rate": diffInfo.TxCompressed / seconds,
	}

	reports.LogEvent(reports.CreateEvent("interface_stats", "interface_stats", 1, columns, nil))
}

// calculateDifference determines the difference between the two argumented values
// and then updates the pointer to the previous value with the current value
// FIXME - need to handle integer wrap
func calculateDifference(previous *uint64, current uint64) uint64 {
	diff := (current - *previous)
	*previous = current
	return diff
}

// getInterfaceIDValue is called to get the interface ID value that corresponds
// to the argumented interface name. It is used to map the system interface names
// we read from /proc/net/dev to the interface ID values we assign to each device
func getInterfaceIDValue(name string) int {
	var val *interfaceDetail

	interfaceInfoLocker.RLock()
	val = interfaceInfoMap[name]
	interfaceInfoLocker.RUnlock()

	if val != nil {
		return val.interfaceID
	}

	return -1
}

// loadInterfaceInfoMap creates a map of interface name to MFW interface ID values
func loadInterfaceInfoMap() {
	networkJSON, err := settings.GetSettings([]string{"network", "interfaces"})
	if networkJSON == nil || err != nil {
		logger.Warn("Unable to read network settings\n")
	}

	networkSlice, ok := networkJSON.([]interface{})
	if !ok {
		logger.Warn("Unable to locate interfaces")
		return
	}

	interfaceInfoLocker.Lock()
	defer interfaceInfoLocker.Unlock()

	// start with an empty map
	interfaceInfoMap = make(map[string]*interfaceDetail)

	// walk the list of interfaces and store each name and ID in the map
	for _, value := range networkSlice {
		item, ok := value.(map[string]interface{})
		if !ok {
			logger.Warn("Invalid interface in settings: %T\n", value)
			continue
		}
		if item == nil {
			logger.Warn("nil interface in interface list\n")
			continue
		}
		// Ignore hidden interfaces
		hid, found := item["hidden"]
		if found && hid.(bool) {
			continue
		}
		// We at least need the device name and interface ID
		if item["device"] == nil || item["interfaceId"] == nil {
			continue
		}

		// create a detail holder for the interface
		holder := new(interfaceDetail)
		holder.interfaceID = int(item["interfaceId"].(float64))
		holder.deviceName = item["device"].(string)

		// grab the wan flag for the interface
		wan, found := item["wan"]
		if found && wan.(bool) {
			holder.wanFlag = true
		}

		// put the interface details in the map
		interfaceInfoMap[holder.deviceName] = holder
	}
}

// refreshActivePingInfo adds details for each WAN interface that we
// use to do our active ping latency checks
func refreshActivePingInfo() {
	facelist, err := net.Interfaces()
	if err != nil {
		return
	}

	interfaceInfoLocker.Lock()
	defer interfaceInfoLocker.Unlock()

	for _, item := range facelist {
		// ignore interfaces not in our map
		if interfaceInfoMap[item.Name] == nil {
			continue
		}

		// found in the map so clear existing values
		interfaceInfoMap[item.Name].netAddress = ""
		interfaceInfoMap[item.Name].pingMode = protoIGNORE

		// ignore interfaces not flagged as WAN in our map
		if interfaceInfoMap[item.Name].wanFlag == false {
			continue
		}

		// ignore if we can't get the address list
		nets, err := item.Addrs()
		if err != nil {
			continue
		}

		// look for the first IPv4 address
		for _, addr := range nets {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			// we ignore anything that isn't an IPv4 address
			if ip.To4() == nil {
				continue
			}
			interfaceInfoMap[item.Name].netAddress = ip.String()
			interfaceInfoMap[item.Name].pingMode = protoICMP4
			logger.Trace("Adding IPv4 active ping interface: %v\n", ip)
			break
		}

		// if we found an IPv4 address for the interface we are finished
		if interfaceInfoMap[item.Name].pingMode != protoIGNORE {
			continue
		}

		// we didn't find an IPv4 address so try again
		for _, addr := range nets {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			// this time we ignore IPv4 addresses
			if ip.To4() != nil {
				continue
			}
			interfaceInfoMap[item.Name].netAddress = ip.String()
			interfaceInfoMap[item.Name].pingMode = protoICMP6
			logger.Trace("Adding IPv6 active ping interface: %v\n", ip)
			break
		}
	}
}

func pingTask() {

	for {
		select {
		case <-pingChannel:
			pingChannel <- true
			return
		case <-time.After(time.Second * time.Duration(pingCheckIntervalSec)):
			refreshActivePingInfo()
			interfaceInfoLocker.RLock()
			for _, value := range interfaceInfoMap {
				if value.pingMode == protoIGNORE {
					continue
				}
				collectPingSample(value)
			}
			interfaceInfoLocker.RUnlock()
		}
	}
}

func collectPingSample(detail *interfaceDetail) {
	logger.Debug("Pinging %s with interfaceDetail[%v]\n", pingCheckTarget, *detail)

	duration, err := pingNetworkAddress(detail.pingMode, detail.netAddress, pingCheckTarget)

	if err != nil {
		logger.Warn("Error returned from pingIPv4Address: %v\n", err)
	}

	statsLocker[detail.interfaceID].Lock()
	statsCollector[detail.interfaceID].AddDataPoint(float64(duration.Nanoseconds()) / 1000000.0)
	logger.Debug("Logging periodic sample: %d, %v, %v ms\n", detail.interfaceID, detail.netAddress, (duration.Nanoseconds() / 1000000))
	statsLocker[detail.interfaceID].Unlock()
}

// We guesstimate the hop count based on the most common TTL values
// which are 32, 64, 128, and 255
// Article: Using abnormal TTL values to detect malicious IP packets
// Ryo Yamada and Shigeki Goto
// http://journals.sfu.ca/apan/index.php/apan/article/download/14/5
func logHopCount(ctid uint32, mess dispatch.NfqueueMessage, name string) {
	var hops uint8
	var ttl uint8

	// we only look for TTL in IPv4 and IPv6 packets
	if mess.IP4Layer != nil {
		ttl = mess.IP4Layer.TTL
	} else if mess.IP6Layer != nil {
		ttl = mess.IP6Layer.HopLimit
	} else {
		return
	}

	if ttl <= 32 {
		hops = (32 - ttl)
	} else if (ttl > 32) && (ttl <= 64) {
		hops = (64 - ttl)
	} else if (ttl > 64) && (ttl <= 128) {
		hops = (128 - ttl)
	} else {
		hops = (255 - ttl)
	}

	// put the hop count in the dictionary
	dict.AddSessionEntry(ctid, name, hops)

	columns := map[string]interface{}{
		"session_id": mess.Session.GetSessionID(),
	}

	modifiedColumns := make(map[string]interface{})
	modifiedColumns[name] = hops

	reports.LogEvent(reports.CreateEvent(name, "sessions", 2, columns, modifiedColumns))
}
