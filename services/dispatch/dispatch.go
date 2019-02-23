// Package dispatch provides dispatching of network/kernel events to various subscribers
// It provides an API for plugins to subscribe to for 3 types of network events
// 1) NFqueue (netfilter queue) packets
// 2) Conntrack events (New, Update, Destroy)
// 3) Netlogger events (from NFLOG target)
// The dispatch will register global callbacks with the kernel package
// and then dispatch events to subscribers accordingly
package dispatch

import (
	"net"
	"sync"
	"time"

	"github.com/untangle/packetd/services/kernel"
	"github.com/untangle/packetd/services/logger"
)

// SubscriptionHolder stores the details of a data callback subscription
type SubscriptionHolder struct {
	Owner         string
	Priority      int
	NfqueueFunc   NfqueueHandlerFunction
	ConntrackFunc ConntrackHandlerFunction
	NetloggerFunc NetloggerHandlerFunction
}

// list of subscribers to each of the three data sources
var nfqueueSubList map[string]SubscriptionHolder
var conntrackSubList map[string]SubscriptionHolder
var netloggerSubList map[string]SubscriptionHolder

// mutexes to protect each of the subscription lists
var nfqueueSubMutex sync.Mutex
var conntrackSubMutex sync.Mutex
var netloggerSubMutex sync.Mutex

// maps to hold the netfilter and conntrack cleanup lists returned from warehouse playback
var nfCleanupList map[uint32]bool
var ctCleanupList map[uint32]bool
var cleanupMutex sync.Mutex

// channel used to shutdown the cleaner task
var shutdownCleanerTask = make(chan bool)

// stores the interval of conntrack updates
var conntrackIntervalSeconds int

// Startup starts the event handling service
func Startup(ctInterval int) {
	conntrackIntervalSeconds = ctInterval

	// create the session, conntrack, and certificate tables
	sessionTable = make(map[uint32]*Session)
	conntrackTable = make(map[uint32]*Conntrack)

	// create the nfqueue, conntrack, and netlogger subscription tables
	nfqueueSubList = make(map[string]SubscriptionHolder)
	conntrackSubList = make(map[string]SubscriptionHolder)
	netloggerSubList = make(map[string]SubscriptionHolder)

	// initialize the sessionIndex counter
	// highest 16 bits are zero
	// middle  32 bits should be epoch
	// lowest  16 bits are zero
	// this means that sessionIndex should be ever increasing despite restarts
	// (unless there are more than 16 bits or 65k sessions per sec on average)
	sessionIndex = ((uint64(time.Now().Unix()) & 0xFFFFFFFF) << 16)

	kernel.RegisterConntrackCallback(conntrackCallback)
	kernel.RegisterNfqueueCallback(nfqueueCallback)
	kernel.RegisterNetloggerCallback(netloggerCallback)

	// start cleaner tasks to clean tables
	go cleanerTask()
}

// Shutdown stops the event handling service
func Shutdown() {
	// Send shutdown signal to periodicTask and wait for it to return
	shutdownCleanerTask <- true
	select {
	case <-shutdownCleanerTask:
	case <-time.After(10 * time.Second):
		logger.Err("Failed to properly shutdown cleanerTask\n")
	}
}

// cleanerTask is a periodic task to cleanup conntrack and session tables
func cleanerTask() {
	var counter int

	for {
		select {
		case <-shutdownCleanerTask:
			shutdownCleanerTask <- true
			return
		case <-time.After(60 * time.Second):
			counter++
			logger.Debug("Calling cleaner task %d\n", counter)
			cleanSessionTable()
			cleanConntrackTable()
		}
	}
}

//dupIP makes a copy of a net.IP
func dupIP(ip net.IP) net.IP {
	dup := make(net.IP, len(ip))
	copy(dup, ip)
	return dup
}

// InsertNfqueueSubscription adds a subscription for receiving nfqueue messages
func InsertNfqueueSubscription(owner string, priority int, function NfqueueHandlerFunction) {
	var holder SubscriptionHolder
	logger.Info("Adding NFQueue Event Subscription (%s, %d)\n", owner, priority)

	holder.Owner = owner
	holder.Priority = priority
	holder.NfqueueFunc = function
	nfqueueSubMutex.Lock()
	_, existing := nfqueueSubList[owner]
	nfqueueSubList[owner] = holder
	nfqueueSubMutex.Unlock()

	if existing {
		panic("DUPLICATE NFQUEUE SUBSCRIPTION DETECTED!")
	}
}

// AttachNfqueueSubscriptions attaches active nfqueue subscriptions to the argumented Session
func AttachNfqueueSubscriptions(session *Session) {
	session.subLocker.Lock()
	session.subscriptions = make(map[string]SubscriptionHolder)

	for index, element := range nfqueueSubList {
		session.subscriptions[index] = element
	}
	session.subLocker.Unlock()
}

// MirrorNfqueueSubscriptions creates a copy of the subscriptions for the argumented Session
func MirrorNfqueueSubscriptions(session *Session) map[string]SubscriptionHolder {
	mirror := make(map[string]SubscriptionHolder)
	session.subLocker.Lock()

	for k, v := range session.subscriptions {
		mirror[k] = v
	}

	session.subLocker.Unlock()
	return (mirror)
}

// InsertConntrackSubscription adds a subscription for receiving conntrack messages
func InsertConntrackSubscription(owner string, priority int, function ConntrackHandlerFunction) {
	var holder SubscriptionHolder
	logger.Info("Adding Conntrack Event Subscription (%s, %d)\n", owner, priority)

	holder.Owner = owner
	holder.Priority = priority
	holder.ConntrackFunc = function
	conntrackSubMutex.Lock()
	conntrackSubList[owner] = holder
	conntrackSubMutex.Unlock()
}

// InsertNetloggerSubscription adds a subscription for receiving netlogger messages
func InsertNetloggerSubscription(owner string, priority int, function NetloggerHandlerFunction) {
	var holder SubscriptionHolder
	logger.Info("Adding Netlogger Event Subscription (%s, %d)\n", owner, priority)

	holder.Owner = owner
	holder.Priority = priority
	holder.NetloggerFunc = function
	netloggerSubMutex.Lock()
	netloggerSubList[owner] = holder
	netloggerSubMutex.Unlock()
}

// HandleWarehousePlayback spins up a goroutine that will playback a warehouse capture
// file, wait until the playback is finished, and save the netfilter and conntrack
// cleanup lists that are returned from the playback function
func HandleWarehousePlayback() {
	go func() {
		cleanupMutex.Lock()
		defer cleanupMutex.Unlock()
		nfCleanupList = make(map[uint32]bool)
		ctCleanupList = make(map[uint32]bool)
		kernel.WarehousePlaybackFile(nfCleanupList, ctCleanupList)
	}()
}

// HandleWarehouseCleanup removes the nfqueue and conntrack entries that
// were created by the previous warehouse playback operation
func HandleWarehouseCleanup() {
	cleanupMutex.Lock()
	defer cleanupMutex.Unlock()

	if nfCleanupList != nil {
		for ctid := range nfCleanupList {
			logger.Debug("Removing playback session for %d\n", ctid)
			sess := findSession(ctid)
			if sess != nil {
				sess.flushDict()
				sess.removeFromSessionTable()
			}
		}
		nfCleanupList = nil
	}

	if ctCleanupList != nil {
		for ctid := range ctCleanupList {
			logger.Debug("Removing playback conntrack for %d\n", ctid)
			removeConntrack(ctid)
		}
		ctCleanupList = nil
	}
}

// GetConntrackTable table
// Note: this returns a copy of the table, but with the same pointers
// do not modify the values in the conntrack entries
func GetConntrackTable() map[uint32]*Conntrack {
	newMap := make(map[uint32]*Conntrack)

	conntrackTableMutex.Lock()
	defer conntrackTableMutex.Unlock()

	for k, v := range conntrackTable {
		newMap[k] = v
	}
	return newMap
}
