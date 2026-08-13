package chathub

import "testing"

func TestUnseenSnapshotHandlesCumulativeAndTruncatedUpdates(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		snapshot string
		want     string
	}{
		{name: "cumulative", previous: "先说结论", snapshot: "先说结论，然后解释", want: "，然后解释"},
		{name: "snapshot starts at previous suffix", previous: "前面的内容。最后一句", snapshot: "最后一句。补充说明", want: "。补充说明"},
		{name: "snapshot contains previous", previous: "最后一句", snapshot: "前缀最后一句。补充", want: "。补充"},
		{name: "single-rune boundary is preserved", previous: "前面的内容。", snapshot: "。最后一句", want: "。最后一句"},
		{name: "stale suffix", previous: "前面的内容。最后一句", snapshot: "最后一句", want: ""},
		{name: "single-rune incremental overlap", previous: "fooa", snapshot: "apple", want: "apple"},
		{name: "no overlap", previous: "已经发送", snapshot: "全新的段落", want: "全新的段落"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unseenSnapshot(tt.previous, tt.snapshot); got != tt.want {
				t.Fatalf("unseenSnapshot(%q, %q) = %q, want %q", tt.previous, tt.snapshot, got, tt.want)
			}
		})
	}
}

func TestUnseenSnapshotDoesNotSplitUTF8(t *testing.T) {
	previous := "已经发送两个🙂🙂"
	snapshot := "🙂🙂然后继续"
	if got, want := unseenSnapshot(previous, snapshot), "然后继续"; got != want {
		t.Fatalf("unseenSnapshot split UTF-8: got %q, want %q", got, want)
	}
}
