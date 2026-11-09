package macverify

import "testing"

func TestPacketFilterRules(t *testing.T) {
	t.Parallel()
	want := packetFilterHeader +
		"block drop out quick user fvslot1\nblock drop in quick user fvslot1\n" +
		"block drop out quick user fvslot2\nblock drop in quick user fvslot2\n"
	if got := string(PacketFilterRules([]string{"fvslot2", "fvslot1", "fvslot2"})); got != want {
		t.Fatalf("PacketFilterRules =\n%s\nwant\n%s", got, want)
	}
}
