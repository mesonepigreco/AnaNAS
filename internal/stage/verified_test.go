package stage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"nas-sync/internal/hash"
)

func TestVerifiedProducerCannotPublishFailedOutput(t *testing.T) {
	for _, mode := range []string{"short", "long", "wrong digest", "late error", "ignored error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, path := newStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := s.ReceiveVerified(ctx, testID, 4, hash.SumBytes([]byte("good")), func(w io.Writer) error {
				switch mode {
				case "short":
					_, err := io.WriteString(w, "go")
					return err
				case "long":
					_, err := io.WriteString(w, "good extra")
					return err
				case "wrong digest":
					_, err := io.WriteString(w, "evil")
					return err
				case "ignored error":
					io.WriteString(w, "too long")
					io.WriteString(w, "good")
					return nil
				case "canceled":
					io.WriteString(w, "good")
					cancel()
					return nil
				default:
					io.WriteString(w, "good")
					return errors.New("failed final trailer")
				}
			})
			if err == nil {
				t.Fatal("failed producer returned success")
			}
			for _, suffix := range []string{".partial", ".ready"} {
				if _, err := os.Lstat(filepath.Join(path, testID+suffix)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed staging survived", suffix, err)
				}
			}
		})
	}
}

func TestVerifiedProducerReadyAndExclusiveRetry(t *testing.T) {
	s, _ := newStore(t)
	calls := 0
	produce := func(w io.Writer) error { calls++; _, err := io.WriteString(w, "good"); return err }
	if err := s.ReceiveVerified(context.Background(), testID, 4, hash.SumBytes([]byte("good")), produce); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenReady(testID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil || string(data) != "good" {
		t.Fatal(string(data), err)
	}
	if err := s.ReceiveVerified(context.Background(), testID, 4, hash.SumBytes([]byte("good")), produce); !errors.Is(err, os.ErrExist) || calls != 1 {
		t.Fatal("duplicate consumed producer", calls, err)
	}
}
