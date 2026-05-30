package apierr

import "testing"

func TestStatusMapping(t *testing.T) {
	cases := map[*Error]int{
		New(NotFound, "x"):     404,
		New(Conflict, "x"):     409,
		New(Validation, "x"):   422,
		New(Unauthorized, "x"): 401,
		New(Forbidden, "x"):    403,
		New(BadRequest, "x"):   400,
		New(Internal, "x"):     500,
	}
	for e, want := range cases {
		if got := e.HTTPStatus(); got != want {
			t.Errorf("code %s: status %d, want %d", e.Code, got, want)
		}
	}
}

func TestAsError(t *testing.T) {
	var generic error = New(NotFound, "missing")
	e, ok := As(generic)
	if !ok || e.Code != NotFound {
		t.Fatalf("As failed: %+v ok=%v", e, ok)
	}
	if _, ok := As(errString("plain")); ok {
		t.Fatal("plain error should not map")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
