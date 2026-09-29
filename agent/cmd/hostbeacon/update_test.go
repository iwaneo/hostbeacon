package main

import (
	"context"
	"errors"
	"testing"

	"github.com/iwaneo/hostbeacon/agent/internal/release"
	"github.com/iwaneo/hostbeacon/agent/internal/release/releasetest"
)

func TestNewestVersionIsReadOnlyFromASignedRelease(t *testing.T) {
	key, other := releasetest.NewKey(), releasetest.NewKey()
	keys, _ := release.ParseKeys(key.Public)
	sums := releasetest.Sums(map[string][]byte{"hostbeacon_1.4.0_amd64.deb": []byte("package")})
	files := map[string][]byte{release.LatestURL(release.SumsFile): sums}
	get := func(_ context.Context, url string) ([]byte, error) {
		if data, found := files[url]; found {
			return data, nil
		}
		return nil, errors.New("404 Not Found")
	}

	if _, err := newestVersion(context.Background(), get, keys); err == nil {
		t.Fatal("read a version without a signature")
	}
	files[release.LatestURL(release.SignatureFile)] = other.Sign(sums, "hostbeacon 1.4.0")
	if _, err := newestVersion(context.Background(), get, keys); err == nil {
		t.Fatal("read a version signed with another key")
	}
	files[release.LatestURL(release.SignatureFile)] = key.Sign(sums, "hostbeacon 1.4.0")
	if got, err := newestVersion(context.Background(), get, keys); err != nil || got != "1.4.0" {
		t.Fatalf("newestVersion = %q, %v", got, err)
	}
}
