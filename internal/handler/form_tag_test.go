package handler

import (
	"reflect"
	"testing"
)

// Request bodies for write endpoints accept only JSON.
//
// A cross-site form submission is a "simple request" in CORS terms: it skips the preflight, the browser sends it anyway, and it attaches
// any Basic Auth credentials the browser has cached for that origin, which makes it the most convenient carrier for CSRF. csrfGuardMiddleware is the main defense,
// and "no form tag on the struct" is the second layer: even if the middleware is bypassed or misconfigured, a form-encoded request can only
// bind to an empty struct, which then fails the required-field validation.
//
// The UI already submits JSON (FormData is used only to read form fields in place), so this does not affect the existing frontend.
func TestWriteRequestStructsRejectFormBinding(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
	}{
		{"AddRunnerRequest", reflect.TypeOf(AddRunnerRequest{})},
		{"UpdateRunnerRequest", reflect.TypeOf(UpdateRunnerRequest{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < tc.typ.NumField(); i++ {
				f := tc.typ.Field(i)
				if tag, ok := f.Tag.Lookup("form"); ok {
					t.Errorf("%s.%s has a form tag (%q): write endpoints accept only JSON, "+
						"form encoding would reopen the cross-site submission hole", tc.name, f.Name, tag)
				}
				// The json tag is required in the reverse direction, otherwise a change in field-name casing would silently alter the API
				if _, ok := f.Tag.Lookup("json"); !ok {
					t.Errorf("%s.%s is missing a json tag", tc.name, f.Name)
				}
			}
		})
	}
}
