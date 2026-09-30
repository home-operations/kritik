package review

import "testing"

func TestShortSHA(t *testing.T) {
	if ShortSHA("0123456789") != "0123456" || ShortSHA("abc") != "abc" {
		t.Fatal("ShortSHA")
	}
}
