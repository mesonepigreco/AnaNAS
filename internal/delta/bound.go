package delta

import "fmt"

// WireBound bounds the wire bytes for a target up to 8 GiB. Every content frame
// advances the target by at least one byte and costs at most 45 framing bytes;
// literals together cannot exceed the target. Header and terminator are fixed.
// This prevents a tiny snapshot reserving the entire global operation allowance.
func WireBound(size int64) (int64, error) {
	if size < 0 || size > 8<<30 {
		return 0, fmt.Errorf("bounded delta target required")
	}
	return size + min(size, int64(MaxOperations))*45 + HeaderBytes + 33, nil
}
