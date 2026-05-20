package proxy

import "testing"

func TestNormalizeTranscodePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "native",
			in:   "/streambridge/transcode/833384/master.m3u8",
			want: "/streambridge/transcode/833384/master.m3u8",
			ok:   true,
		},
		{
			name: "emby prefixed",
			in:   "/emby/streambridge/transcode/833384/master.m3u8",
			want: "/streambridge/transcode/833384/master.m3u8",
			ok:   true,
		},
		{
			name: "emby prefixed absolute url",
			in:   "/emby/http://127.0.0.1:8097/streambridge/transcode/833384/master.m3u8",
			want: "/streambridge/transcode/833384/master.m3u8",
			ok:   true,
		},
		{
			name: "not transcode",
			in:   "/emby/System/Info",
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeTranscodePath(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v", ok)
			}
			if got != tt.want {
				t.Fatalf("path = %q", got)
			}
		})
	}
}
