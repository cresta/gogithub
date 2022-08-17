package gogithub

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestExpireCacheExpire(t *testing.T) {
	c := ExpireCache[string, int]{
		DefaultExpiry: time.Millisecond,
	}
	c.Set("a", 1)
	time.Sleep(time.Millisecond * 2)
	_, exists := c.Get("a")
	require.False(t, exists)
}

func TestExpireCache_Set(t *testing.T) {
	c := ExpireCache[string, int]{
		DefaultExpiry: time.Hour,
	}
	c.Set("a", 1)
	val, exists := c.Get("a")
	require.True(t, exists)
	require.Equal(t, 1, val)
}
