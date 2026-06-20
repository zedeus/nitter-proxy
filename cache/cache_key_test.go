package cache

import (
	"net/url"
	"testing"
)

func TestBuildCacheKey_FeaturesIgnored(t *testing.T) {
	u1, _ := url.Parse(`https://api.x.com/graphql/abc/Test?variables={"id":"123"}&features={"f1":true}`)
	u2, _ := url.Parse(`https://api.x.com/graphql/abc/Test?variables={"id":"123"}&features={"f2":false}`)
	u3, _ := url.Parse(`https://api.x.com/graphql/abc/Test?variables={"id":"123"}`)

	key1 := BuildCacheKey(u1)
	key2 := BuildCacheKey(u2)
	key3 := BuildCacheKey(u3)

	if key1 != key2 || key2 != key3 {
		t.Errorf("features should be ignored:\nkey1=%s\nkey2=%s\nkey3=%s", key1, key2, key3)
	}
}

func TestBuildCacheKey_VariablesAffectKey(t *testing.T) {
	u1, _ := url.Parse(`https://api.x.com/graphql/abc/Test?variables={"id":"123"}`)
	u2, _ := url.Parse(`https://api.x.com/graphql/abc/Test?variables={"id":"456"}`)

	if BuildCacheKey(u1) == BuildCacheKey(u2) {
		t.Error("different variables should produce different keys")
	}
}

func TestBuildCacheKey_Deterministic(t *testing.T) {
	u, _ := url.Parse("https://api.x.com/graphql/abc/UserByScreenName?variables={}")
	if BuildCacheKey(u) != BuildCacheKey(u) {
		t.Error("cache keys should be deterministic")
	}
}

func TestExtractEndpoint(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"/graphql/abc123/UserByScreenName", "UserByScreenName"},
		{"/graphql/xyz789/TweetDetail", "TweetDetail"},
		{"/1.1/search/tweets.json", "tweets.json"},
		{"/2/users/123", "123"},
		{"/", ""},
	}

	for _, tt := range tests {
		u, _ := url.Parse("https://api.twitter.com" + tt.path)
		if got := ExtractEndpoint(u); got != tt.expected {
			t.Errorf("ExtractEndpoint(%s) = %s, want %s", tt.path, got, tt.expected)
		}
	}
}
