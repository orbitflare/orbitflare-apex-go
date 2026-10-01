package apex

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/quic-go/quic-go"
)

const rustCertForTestKey = "3081f63081a9a00302010202080101010101010101300506032b657030163114301206035504030c0b536f6c616e61206e6f64653020170d3730303130313030303030305a180f34303936303130313030303030305a3000302a300506032b65700321001ae09b747ce23320cf0c6960c8c126ae89c34fbc79e9e3732c158f6dc5950015a329302730170603551d110101ff040d300b82096c6f63616c686f7374300c0603551d130101ff04023000300506032b6570034100ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

func TestDerivationMatchesTheServer(t *testing.T) {
	got := ClientPubkey("test-api-key").String()
	if got != "2ovF9aU8fszHXpZwxaC1S9NF2m5VVDRBUvDUVQvXs4Tv" {
		t.Fatalf("derived %s", got)
	}
}

func TestCertificateIsByteIdenticalToTheRustClient(t *testing.T) {
	key := deriveClientKey("test-api-key")
	got := hex.EncodeToString(dummyCertificate(key.Public().(ed25519.PublicKey)))
	if got != rustCertForTestKey {
		t.Fatalf("certificate differs from solana-tls-utils\n got %s\nwant %s", got, rustCertForTestKey)
	}
}

func TestTLSConfigPresentsTheDerivedCertificate(t *testing.T) {
	key := deriveClientKey("k")
	conf := clientTLSConfig(key, "fra.apex.orbitflare.com", nil)
	cert, err := conf.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cert.Certificate[0], key.Public().(ed25519.PublicKey)) {
		t.Fatal("certificate does not carry the derived public key")
	}
	if conf.NextProtos[0] != "solana-tpu" || !conf.InsecureSkipVerify {
		t.Fatalf("unexpected tls config: %+v", conf)
	}
}

func TestPacketLayoutIsBincodeOfTransactionPacket(t *testing.T) {
	got := EncodePacket([]byte{9, 9, 9}, true, RetryBudget(7))
	want := []byte{3, 0, 0, 0, 0, 0, 0, 0, 9, 9, 9, 1, 1, 7, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = EncodePacket([]byte{1}, false, nil)
	want = []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestAdmissionRoundTrip(t *testing.T) {
	ok := append([]byte{0}, bytes.Repeat([]byte{5}, 64)...)
	a, decoded := DecodeAdmission(ok)
	var sig solana.Signature
	copy(sig[:], bytes.Repeat([]byte{5}, 64))
	if !decoded || !a.Accepted || a.Signature != sig {
		t.Fatalf("accepted frame decoded as %+v", a)
	}
	rej := append([]byte{5, 3, 0}, []byte("low")...)
	a, decoded = DecodeAdmission(rej)
	if !decoded || a.Accepted || a.Code != AdmissionBelowFloor || a.Message != "low" {
		t.Fatalf("rejected frame decoded as %+v", a)
	}
	if _, decoded := DecodeAdmission(nil); decoded {
		t.Fatal("empty frame decoded")
	}
	if _, decoded := DecodeAdmission([]byte{0, 1, 2}); decoded {
		t.Fatal("short accepted frame decoded")
	}
	if _, decoded := DecodeAdmission([]byte{3, 9, 0, 'x'}); decoded {
		t.Fatal("rejected frame with a short message decoded")
	}
	if a, _ := DecodeAdmission([]byte{200, 0, 0}); a.Code != AdmissionUnknown {
		t.Fatalf("unknown code decoded as %v", a.Code)
	}
}

func TestEveryRegionParsesBackFromItsCode(t *testing.T) {
	for _, r := range AllRegions {
		got, ok := ParseRegion(r.Code())
		if !ok || got != r {
			t.Fatalf("%s parsed as %v", r.Code(), got)
		}
	}
	if r, ok := ParseRegion("Dublin"); !ok || r != Dublin {
		t.Fatal("Dublin did not parse")
	}
	if Dublin.QUICEndpoint() != "dub.apex.orbitflare.com:7001" {
		t.Fatal(Dublin.QUICEndpoint())
	}
	if Global.RPCURL() != "http://global.apex.orbitflare.com" {
		t.Fatal(Global.RPCURL())
	}
	if _, ok := ParseRegion("mumbai"); ok {
		t.Fatal("unknown region parsed")
	}
}

func TestANewAddressIsTakenOnlyInTheFamilyTheSocketCanDial(t *testing.T) {
	current := netip.MustParseAddrPort("192.0.2.15:7001")
	v6 := netip.MustParseAddrPort("[2001:db8::5]:7001")
	moved := netip.MustParseAddrPort("203.0.113.65:7001")
	if got, ok := sameFamily([]netip.AddrPort{v6, moved}, current); !ok || got != moved {
		t.Fatalf("got %v", got)
	}
	if _, ok := sameFamily([]netip.AddrPort{v6}, current); ok {
		t.Fatal("took an address the socket cannot dial")
	}
	if _, ok := sameFamily(nil, current); ok {
		t.Fatal("took an address from nothing")
	}
}

func TestTooLargeAndRejectedErrors(t *testing.T) {
	var tooLarge *TooLargeError
	if !errors.As(fmt.Errorf("x: %w", &TooLargeError{Size: 5000}), &tooLarge) || tooLarge.Size != 5000 {
		t.Fatal("TooLargeError does not unwrap")
	}
	err := &RejectedError{Code: AdmissionNoTip, Message: "no tip"}
	if err.Error() != "apex: rejected (NoTip): no tip" {
		t.Fatal(err.Error())
	}
	if !errors.Is(wrap(ErrConnect, errors.New("boom")), ErrConnect) {
		t.Fatal("wrapped error lost its kind")
	}
}

func TestTipHelpers(t *testing.T) {
	payer := solana.NewWallet().PublicKey()
	tip := solana.MustPublicKeyFromBase58("APeX2oLtjYehgTMUCA971L8htM7tGNqsXHDz5NrivhhX")
	ix := TipInstruction(payer, tip, MinTipLamports)
	if !ix.ProgramID().Equals(solana.SystemProgramID) {
		t.Fatal("tip is not a system transfer")
	}
	accounts := ix.Accounts()
	if !accounts[0].PublicKey.Equals(payer) || !accounts[0].IsSigner || !accounts[1].PublicKey.Equals(tip) || !accounts[1].IsWritable {
		t.Fatalf("unexpected accounts %+v", accounts)
	}
	if _, ok := PickTipAccount(nil); ok {
		t.Fatal("picked from an empty list")
	}
	if got, ok := PickTipAccount([]solana.PublicKey{tip}); !ok || got != tip {
		t.Fatal("did not pick the only account")
	}
}

func TestOnlyAnApplicationCloseByTheEndpointIsARefusal(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&quic.ApplicationError{Remote: true, ErrorCode: 2}, true},
		{&quic.ApplicationError{Remote: false, ErrorCode: 2}, false},
		{&quic.TransportError{Remote: true, ErrorCode: quic.ApplicationErrorErrorCode}, true},
		{&quic.TransportError{Remote: true, ErrorCode: quic.ProtocolViolation}, false},
		{&quic.IdleTimeoutError{}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := closedByEndpoint(tc.err); got != tc.want {
			t.Fatalf("%T %v: got %v", tc.err, tc.err, got)
		}
	}
}
