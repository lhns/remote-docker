package proxy

import "testing"

func TestParseMountFailure(t *testing.T) {
	const refused = `{"message":"error while mounting volume '/var/lib/docker/volumes/rd-1a2b3c4d-0f9e8d7c6b5a4f3e/_data': failed to mount local volume: mount :/m/0f9e8d7c6b5a4f3e:/var/lib/docker/volumes/rd-1a2b3c4d-0f9e8d7c6b5a4f3e/_data, data: addr=127.0.0.1,port=40123,nfsvers=3,proto=tcp: connection refused"}`

	tests := []struct {
		name string
		body string
		want MountFailure
		ok   bool
	}{
		{"refused", refused, MountFailure{"rd-1a2b3c4d-0f9e8d7c6b5a4f3e", "1a2b3c4d", 40123}, true},
		{"other error", `{"message":"No such container: abc"}`, MountFailure{}, false},
		{"not refused", `{"message":"error while mounting volume '/var/lib/docker/volumes/rd-1a2b3c4d-x/_data': data: addr=127.0.0.1,port=40123: timed out"}`, MountFailure{}, false},
		{"no port", `{"message":"error while mounting volume '/var/lib/docker/volumes/rd-1a2b3c4d-x/_data': connection refused"}`, MountFailure{}, false},
		{"not ours", `{"message":"error while mounting volume '/var/lib/docker/volumes/data/_data': port=2049: connection refused"}`, MountFailure{}, false},
		{"short client id", `{"message":"volumes/rd-abc-x/_data port=1: connection refused"}`, MountFailure{}, false},
		{"not json", `connection refused port=1 volumes/rd-1a2b3c4d-x/_data`, MountFailure{}, false},
		{"empty", ``, MountFailure{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseMountFailure([]byte(tt.body))
			if ok != tt.ok || got != tt.want {
				t.Errorf("parseMountFailure = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestRewriteMessageKeepsOtherFields(t *testing.T) {
	got, err := rewriteMessage([]byte(`{"message":"old","extra":[1,2]}`), "new\n  fix: x")
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"extra":[1,2],"message":"new\n  fix: x"}`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
