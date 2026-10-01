package apex

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
)

const (
	MaxTransactionSize = 4096
	MaxAdmissionFrame  = 1 + 2 + 512
)

func FrameParts(length int, mevProtect bool, maxRetries *uint16) ([8]byte, []byte) {
	var header [8]byte
	binary.LittleEndian.PutUint64(header[:], uint64(length))
	trailer := make([]byte, 0, 4)
	if mevProtect {
		trailer = append(trailer, 1)
	} else {
		trailer = append(trailer, 0)
	}
	if maxRetries == nil {
		trailer = append(trailer, 0)
	} else {
		trailer = append(trailer, 1)
		trailer = binary.LittleEndian.AppendUint16(trailer, *maxRetries)
	}
	return header, trailer
}

func EncodePacket(wire []byte, mevProtect bool, maxRetries *uint16) []byte {
	header, trailer := FrameParts(len(wire), mevProtect, maxRetries)
	out := make([]byte, 0, len(header)+len(wire)+len(trailer))
	out = append(out, header[:]...)
	out = append(out, wire...)
	return append(out, trailer...)
}

type AdmissionCode uint8

const (
	AdmissionOK              AdmissionCode = 0
	AdmissionUnauthorized    AdmissionCode = 1
	AdmissionRateLimited     AdmissionCode = 2
	AdmissionInvalid         AdmissionCode = 3
	AdmissionNoTip           AdmissionCode = 4
	AdmissionBelowFloor      AdmissionCode = 5
	AdmissionBusy            AdmissionCode = 6
	AdmissionMalformedPacket AdmissionCode = 7
	AdmissionUnknown         AdmissionCode = 255
)

func AdmissionCodeFromByte(b byte) AdmissionCode {
	if b <= byte(AdmissionMalformedPacket) {
		return AdmissionCode(b)
	}
	return AdmissionUnknown
}

func (c AdmissionCode) String() string {
	switch c {
	case AdmissionOK:
		return "Ok"
	case AdmissionUnauthorized:
		return "Unauthorized"
	case AdmissionRateLimited:
		return "RateLimited"
	case AdmissionInvalid:
		return "Invalid"
	case AdmissionNoTip:
		return "NoTip"
	case AdmissionBelowFloor:
		return "BelowFloor"
	case AdmissionBusy:
		return "Busy"
	case AdmissionMalformedPacket:
		return "MalformedPacket"
	}
	return fmt.Sprintf("Unknown(%d)", uint8(c))
}

type Admission struct {
	Accepted  bool
	Signature solana.Signature
	Code      AdmissionCode
	Message   string
}

func DecodeAdmission(frame []byte) (Admission, bool) {
	if len(frame) == 0 {
		return Admission{}, false
	}
	code, rest := frame[0], frame[1:]
	if code == 0 {
		if len(rest) < 64 {
			return Admission{}, false
		}
		var sig solana.Signature
		copy(sig[:], rest[:64])
		return Admission{Accepted: true, Signature: sig, Code: AdmissionOK}, true
	}
	if len(rest) < 2 {
		return Admission{}, false
	}
	n := int(binary.LittleEndian.Uint16(rest[:2]))
	if len(rest) < 2+n {
		return Admission{}, false
	}
	return Admission{
		Code:    AdmissionCodeFromByte(code),
		Message: strings.ToValidUTF8(string(rest[2:2+n]), "\uFFFD"),
	}, true
}
