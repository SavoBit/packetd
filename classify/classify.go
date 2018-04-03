package classify

//#include "string.h"
//#include "stdlib.h"
//#include "stdarg.h"
//#include "syslog.h"
//#include "stdio.h"
//#include "ctype.h"
//#include "math.h"
//#include "time.h"
//#include "sys/time.h"
//#include "pthread.h"
//#include "navl.h"
//#include "classify.h"
//#cgo LDFLAGS: -lnavl -lm -ldl
import "C"

import "unsafe"
import "sync"

import "github.com/untangle/packetd/support"

func Plugin_Startup(childsync *sync.WaitGroup) {
	support.LogMessage("Plugin_Startup(%s) has been called\n", "classify")
	childsync.Add(1)
	C.vendor_startup()
}

func Plugin_Goodbye(childsync *sync.WaitGroup) {
	support.LogMessage("Plugin_Goodbye(%s) has been called\n", "classify")
	C.vendor_shutdown()
	childsync.Done()
}

func Plugin_netfilter_handler(buffer []byte, length int) {
	ptr := (*C.uchar)(unsafe.Pointer(&buffer[0]))
	C.vendor_classify(ptr, C.int(length))
}

func Plugin_conntrack_handler(tracker *support.Tracker) {
}
