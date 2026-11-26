package cordon

import (
	"strings"
	"testing"
)

func TestTurnEndsSeesOnlyATopLevelResultType(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 1<<20)
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"type first", `{"type":"result","subtype":"success"}` + "\n", 1},
		{"type after other members", `{"subtype":"success","is_error":false,"n":3,"usage":{"type":"x"},"list":[1,{"a":"b"}],"type":"result"}` + "\n", 1},
		{"spaces around the colon", `{ "type" : "result" }` + "\n", 1},
		{"after a megabyte", `{"text":"` + long + `","type":"result"}` + "\n", 1},
		{"two results in one write", `{"type":"result"}` + "\n" + `{"type":"result"}` + "\n", 2},
		{"another type", `{"type":"assistant","message":{"type":"result"}}` + "\n", 0},
		{"a longer word", `{"type":"results"}` + "\n", 0},
		{"a nested result", `{"type":"assistant","message":{"content":[{"type":"result"}]}}` + "\n", 0},
		{"a result inside a string", `{"type":"user","text":"\"type\":\"result\""}` + "\n", 0},
		{"result as a key", `{"result":"type"}` + "\n", 0},
		{"result as a value of another key", `{"kind":"result"}` + "\n", 0},
		{"an escaped key", "{\"ty\\u0070e\":\"result\"}\n", 0},
		{"a top-level array", `["type","result"]` + "\n", 0},
		{"no newline yet", `{"type":"result"}`, 0},
		{"not JSON", "type result\n", 0},
		{"a broken line, then a result", `{"type":"assist` + "\n" + `{"type":"result"}` + "\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, chunk := range []int{len(tc.in), 1, 7} {
				n := 0
				s := newTurnEnds(func() { n++ })
				for in := tc.in; in != ""; {
					size := min(chunk, len(in))
					if wrote, err := s.Write([]byte(in[:size])); err != nil || wrote != size {
						t.Fatalf("Write = %d, %v", wrote, err)
					}
					in = in[size:]
				}
				if n != tc.want {
					t.Fatalf("in chunks of %d: saw %d turn ends, want %d", chunk, n, tc.want)
				}
			}
		})
	}
}
