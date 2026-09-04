package quota

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestFetchErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *FetchError
		want string
	}{
		{
			name: "category only",
			err:  &FetchError{Category: CategoryBadJSON},
			want: "quota: bad-json",
		},
		{
			name: "category and detail",
			err:  &FetchError{Category: CategoryTransport, Detail: "no doer configured"},
			want: "quota: transport: no doer configured",
		},
		{
			name: "category, status and detail",
			err:  &FetchError{Category: CategoryAuth, Status: 401, Detail: "credential rejected"},
			want: "quota: auth (http 401): credential rejected",
		},
		{
			name: "wrapped cause",
			err:  &FetchError{Category: CategoryTimeout, Detail: "request failed", cause: errors.New("deadline")},
			want: "quota: timeout: request failed: deadline",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFetchErrorUnwrap(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", context.DeadlineExceeded)
	err := error(&FetchError{Category: CategoryTimeout, Detail: "request failed", cause: wrapped})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is could not see through the FetchError to its cause")
	}
	if got := errors.Unwrap(err); got != wrapped {
		t.Errorf("Unwrap = %v, want %v", got, wrapped)
	}
	if got := errors.Unwrap(&FetchError{Category: CategoryBadJSON}); got != nil {
		t.Errorf("Unwrap = %v, want nil for a response-derived failure", got)
	}
}

func TestCategory(t *testing.T) {
	if got := Category(nil); got != "" {
		t.Errorf("Category(nil) = %q, want empty", got)
	}
	if got := Category(errors.New("something else")); got != "" {
		t.Errorf("Category of a foreign error = %q, want empty", got)
	}

	wrapped := fmt.Errorf("poll auth-1: %w", &FetchError{Category: CategoryForbidden, Status: 403})
	if got := Category(wrapped); got != CategoryForbidden {
		t.Errorf("Category = %q, want %q", got, CategoryForbidden)
	}
}
