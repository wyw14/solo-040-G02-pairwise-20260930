package waterway

import "fmt"

type waterwayError string

func (e waterwayError) Error() string { return string(e) }

func errWaterway(format string, args ...any) error {
	return waterwayError(fmt.Sprintf(format, args...))
}

// IsDenial reports whether err is a clearance rejection (conflict) rather than
// a malformed request.
func IsDenial(err error) bool {
	_, ok := err.(*DeniedError)
	return ok
}

// DeniedError carries the structured block reason back to callers.
type DeniedError struct {
	Denial Denial
}

func (e *DeniedError) Error() string { return e.Denial.Reason }
