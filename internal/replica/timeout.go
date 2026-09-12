package replica

import "time"

// contentTimeout permits several paced passes over a bounded batch (snapshot,
// wire verification, receive, publication and durability), plus fixed protocol
// overhead. A 2 MiB/s floor models the deployed peer when the local side is
// faster; slower configured local rates remain authoritative.
func contentTimeout(bytes, localRate int64) time.Duration {
	rate := localRate
	if rate > 2<<20 {
		rate = 2 << 20
	}
	seconds := (bytes + rate - 1) / rate
	return 45*time.Second + time.Duration(seconds*8)*time.Second
}
