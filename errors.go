package apex

import (
	"errors"
	"fmt"
)

var (
	ErrResolve       = errors.New("apex: resolve endpoint")
	ErrTLS           = errors.New("apex: tls config")
	ErrBind          = errors.New("apex: bind local socket")
	ErrConnect       = errors.New("apex: connect")
	ErrConnection    = errors.New("apex: connection")
	ErrWrite         = errors.New("apex: write")
	ErrRead          = errors.New("apex: read")
	ErrTimeout       = errors.New("apex: timed out")
	ErrSerialize     = errors.New("apex: serialize transaction")
	ErrBadAdmission  = errors.New("apex: malformed admission response")
	ErrClosed        = errors.New("apex: closed")
	ErrNoSignature   = errors.New("apex: transaction has no signature")
	ErrClientStopped = errors.New("apex: client closed")
)

type TooLargeError struct {
	Size int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("apex: transaction too large: %d bytes (max 4096; legacy and v0 max 1232)", e.Size)
}

type RejectedError struct {
	Code    AdmissionCode
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("apex: rejected (%s): %s", e.Code, e.Message)
}

func wrap(kind error, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", kind, err)
}
