package ops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestYuActionReleaseUsesItsOwnRevisionAndPinsBothImages(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint("frontend mismatch=", mismatch), func(t *testing.T) {
			c := testController(t)
			revision := strings.Repeat("a", 40)
			backendID, frontendID := "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
			var pulls []string
			c.run = func(_ context.Context, args []string, _ string) (string, error) {
				if len(args) == 3 && args[1] == "pull" {
					pulls = append(pulls, args[2])
					return "", nil
				}
				if len(args) == 4 && args[1] == "image" && args[2] == "inspect" {
					id, rev := backendID, revision
					if strings.Contains(args[3], "frontend") || args[3] == frontendID {
						id = frontendID
						if mismatch {
							rev = strings.Repeat("b", 40)
						}
					}
					return string(marshal([]any{object{"Id": id, "Config": object{"Labels": object{
						"org.opencontainers.image.revision": rev,
						"org.opencontainers.image.source":   "https://github.com/CoYumeLabs/DreamTrans",
					}}}})), nil
				}
				return "", fmt.Errorf("unexpected command: %v", args)
			}
			if mismatch {
				requireFailure(t, func() { c.yuactionImages(&options{}) }, "revisions differ")
				return
			}
			backend, frontend := c.yuactionImages(&options{})
			if backend != backendID || frontend != frontendID {
				t.Fatalf("images not pinned: %s %s", backend, frontend)
			}
			want := []string{"ghcr.io/coyumelabs/dreamtrans-yuaction-backend:latest", "ghcr.io/coyumelabs/dreamtrans-yuaction-frontend:sha-" + revision}
			if !slices.Equal(pulls, want) {
				t.Fatalf("release pair: %v, want %v", pulls, want)
			}
		})
	}
}
