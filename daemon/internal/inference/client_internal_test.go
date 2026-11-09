package inference

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// decodeStrictObject refuses a repeated member at any depth through
// strictjson, so model output can't pick which of two copies a gate reads.
func TestDecodeStrictObjectRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	type inner struct {
		C string `json:"c"`
	}
	type object struct {
		A string `json:"a"`
		B inner  `json:"b"`
	}

	for name, payload := range map[string]string{
		"top-level duplicate": `{"a":"x","a":"y","b":{"c":"1"}}`,
		"nested duplicate":    `{"a":"x","b":{"c":"1","c":"2"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var dst object
			if err := decodeStrictObject([]byte(payload), &dst, 1<<10); !errors.Is(err, strictjson.ErrDuplicateName) {
				t.Fatalf("error = %v, want strictjson.ErrDuplicateName", err)
			}
		})
	}

	var dst object
	if err := decodeStrictObject([]byte(`{"a":"x","b":{"c":"1"}}`), &dst, 1<<10); err != nil {
		t.Fatalf("a payload without duplicates was refused: %v", err)
	}
}
