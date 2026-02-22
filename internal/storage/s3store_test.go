package storage

import "testing"

func TestParseAlbumSourceKey(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		wantID string
		wantOK bool
	}{
		{name: "valid", key: "albums/album-a/source.zip", wantID: "album-a", wantOK: true},
		{name: "missing album id", key: "albums//source.zip", wantID: "", wantOK: false},
		{name: "wrong file", key: "albums/album-a/index.json", wantID: "", wantOK: false},
		{name: "wrong prefix", key: "foo/album-a/source.zip", wantID: "", wantOK: false},
		{name: "deep key", key: "albums/album-a/extra/source.zip", wantID: "", wantOK: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotOK := parseAlbumSourceKey(tc.key)
			if gotID != tc.wantID || gotOK != tc.wantOK {
				t.Fatalf("parseAlbumSourceKey(%q) = (%q, %v), want (%q, %v)", tc.key, gotID, gotOK, tc.wantID, tc.wantOK)
			}
		})
	}
}
