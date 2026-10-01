package apex

import (
	"fmt"
	"strings"
)

type Region int

const (
	Frankfurt Region = iota
	Amsterdam
	Dublin
	London
	NewYork
	SaltLakeCity
	Singapore
	Tokyo
	Siauliai
	Global
)

const (
	QUICPort = 7001
	RPCPort  = 80
)

var AllRegions = [...]Region{
	Frankfurt,
	Amsterdam,
	Dublin,
	London,
	NewYork,
	SaltLakeCity,
	Singapore,
	Tokyo,
	Siauliai,
	Global,
}

var regionCodes = [...]string{
	Frankfurt:    "fra",
	Amsterdam:    "ams",
	Dublin:       "dub",
	London:       "lon",
	NewYork:      "nyc",
	SaltLakeCity: "slc",
	Singapore:    "sgp",
	Tokyo:        "tyo",
	Siauliai:     "sqq",
	Global:       "global",
}

func (r Region) Code() string {
	if r < 0 || int(r) >= len(regionCodes) {
		return ""
	}
	return regionCodes[r]
}

func (r Region) String() string {
	return r.Code()
}

func (r Region) Host() string {
	return r.Code() + ".apex.orbitflare.com"
}

func (r Region) QUICEndpoint() string {
	return fmt.Sprintf("%s:%d", r.Host(), QUICPort)
}

func (r Region) RPCURL() string {
	return "http://" + r.Host()
}

func ParseRegion(s string) (Region, bool) {
	switch strings.ToLower(s) {
	case "fra", "frankfurt":
		return Frankfurt, true
	case "ams", "amsterdam":
		return Amsterdam, true
	case "dub", "dublin":
		return Dublin, true
	case "lon", "london":
		return London, true
	case "nyc", "ny", "newyork", "new-york":
		return NewYork, true
	case "slc", "saltlakecity", "salt-lake-city":
		return SaltLakeCity, true
	case "sgp", "sin", "singapore":
		return Singapore, true
	case "tyo", "tokyo":
		return Tokyo, true
	case "sqq", "siauliai":
		return Siauliai, true
	case "global", "auto":
		return Global, true
	}
	return 0, false
}
