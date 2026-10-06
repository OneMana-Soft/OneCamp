package helpers

import (
	"errors"
	"net/url"
	"testing"
)

func TestListPaging(t *testing.T) {
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	if _, _, _, err := ListPaging(q("")); !errors.Is(err, ErrListPaging) {
		t.Errorf("no paging: got %v, want ErrListPaging", err)
	}
	if all, _, _, err := ListPaging(q("getAll=true")); err != nil || !all {
		t.Errorf("getAll: got %v %v", all, err)
	}
	if _, size, index, err := ListPaging(q("pageSize=20&pageIndex=2")); err != nil || size != 20 || index != 2 {
		t.Errorf("paged: got %d %d %v", size, index, err)
	}
	for _, bad := range []string{"pageSize=x&pageIndex=0", "pageSize=10&pageIndex=-1"} {
		if _, _, _, err := ListPaging(q(bad)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}
