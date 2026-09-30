package transferapi

import (
	"testing"
	"time"
)

func TestTransferTimeoutDoesNotAssumeConfiguredDiskSpeedIsNetworkSpeed(t *testing.T) {
	// The wizard installs a 1 GiB/s read ceiling and an 8 GiB batch limit. The
	// previous NAS timeout was only 109 seconds and consistently cut off a
	// 1,020,135,660-byte video over an approximately 8 MiB/s Wi-Fi connection.
	timeout := transferTimeout(8<<30, 1<<30)
	if timeout != transferTimeout(8<<30, 2<<20) {
		t.Fatal("server and client deadlines differ", timeout)
	}
	for _, rate := range []int64{8 << 20, 2 << 20} {
		duration := time.Duration((1020135660+rate-1)/rate) * time.Second
		if timeout <= duration {
			t.Fatal("large file cannot finish", timeout, duration)
		}
	}
	slow := transferTimeout(8<<30, 1<<20)
	if slow <= timeout {
		t.Fatal("slower configured pacing was ignored", slow, timeout)
	}
	if got := transferTimeout(1, 1<<30); got != 53*time.Second {
		t.Fatal("small operations lost their bounded deadline", got)
	}
}
