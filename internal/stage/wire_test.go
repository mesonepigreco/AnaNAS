package stage

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

func TestWireBoundAndProducerErrorLeaveNoReady(t *testing.T) {
	for _, mode := range []string{"oversize", "late", "short"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newStore(t)
			if _, err := s.WriteWire(context.Background(), testID, 100, func(w io.Writer) error {
				if mode == "oversize" {
					w.Write(make([]byte, 101))
					return nil
				}
				if mode == "short" {
					return nil
				}
				w.Write(make([]byte, 100))
				return errors.New("encode rejected target")
			}); err == nil {
				t.Fatal("bad producer accepted")
			}
			if f, err := s.OpenWire(testID); err == nil {
				f.Close()
				t.Fatal("invalid ready spool")
			}
			if err := s.DiscardWirePartial(testID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.OpenReady(testID); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("wire appeared as content", err)
			}
		})
	}
}
