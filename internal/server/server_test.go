package server

import (
	"fmt"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ogen-app/image-service/internal/imgengine"
)

// TestMapEngineErr_RejectReason verifies each terminal engine verdict maps to an
// InvalidArgument carrying the right machine-readable reason as a
// google.rpc.ErrorInfo detail (CON-281). ogen keys off this to distinguish
// vector / unsupported / corrupt / dimensions / too-large without parsing prose.
func TestMapEngineErr_RejectReason(t *testing.T) {
	cases := []struct {
		name       string
		sentinel   error
		wantReason string
	}{
		{"vector", imgengine.ErrVector, reasonVector},
		{"unsupported", imgengine.ErrUnsupported, reasonUnsupportedType},
		{"corrupt", imgengine.ErrCorrupt, reasonCorrupt},
		{"dimensions", imgengine.ErrDimensions, reasonDimensions},
		{"oversize", imgengine.ErrOversize, reasonTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrap to mirror the engine's fmt.Errorf("%w: ...") raise sites.
			gerr := mapEngineErr(fmt.Errorf("%w: detail", tc.sentinel))
			st, ok := status.FromError(gerr)
			if !ok || st.Code() != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", st.Code())
			}
			reason := ""
			for _, d := range st.Details() {
				if info, ok := d.(*errdetails.ErrorInfo); ok {
					if info.GetDomain() != rejectDomain {
						t.Fatalf("domain = %q, want %q", info.GetDomain(), rejectDomain)
					}
					reason = info.GetReason()
				}
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestMapEngineErr_TransientNoReason: a transient/internal fault is not a content
// verdict, so it carries no reject ErrorInfo (ogen retries it).
func TestMapEngineErr_TransientNoReason(t *testing.T) {
	gerr := mapEngineErr(fmt.Errorf("%w: boom", imgengine.ErrFetch))
	st, _ := status.FromError(gerr)
	if st.Code() != codes.Internal {
		t.Fatalf("code = %v, want Internal", st.Code())
	}
	for _, d := range st.Details() {
		if _, ok := d.(*errdetails.ErrorInfo); ok {
			t.Fatal("transient error must not carry a reject ErrorInfo")
		}
	}
}
