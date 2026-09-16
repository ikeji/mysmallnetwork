package netutil

import (
	"reflect"
	"testing"
)

func TestOptionalValueFlag(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"-n", "x", "-l"}, []string{"-n", "x", "-l=auto"}},
		{[]string{"-n", "x", "-l", "1234"}, []string{"-n", "x", "-l=1234"}},
		{[]string{"-l", "-v"}, []string{"-l=auto", "-v"}},
		{[]string{"--l=5"}, []string{"--l=5"}},
		{[]string{"-l", "--", "-l"}, []string{"-l=auto", "--", "-l"}},
	}
	for _, c := range cases {
		if got := OptionalValueFlag(c.in, "l", "auto"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("OptionalValueFlag(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSplitName(t *testing.T) {
	for _, c := range [][3]string{{"a", "a", ""}, {"a:22", "a", "22"}, {"a:h:22", "a", "h:22"}} {
		n, tg := SplitName(c[0])
		if n != c[1] || tg != c[2] {
			t.Errorf("SplitName(%q) = %q,%q", c[0], n, tg)
		}
	}
}
