package types

import (
	"net/netip"
	"strings"
	"time"

	"github.com/expr-lang/expr/vm"
	log "github.com/sirupsen/logrus"

	"github.com/crowdsecurity/crowdsec/pkg/models"
)

const (
	LOG = iota
	OVFLW
	APPSEC
)

// Event is the structure representing a runtime event (log or overflow)
type Event struct {
	/* is it a log or an overflow */
	Type            int    `yaml:"Type,omitempty" json:"Type,omitempty"`             // Can be types.LOG (0) or types.OVFLOW (1)
	ExpectMode      int    `yaml:"ExpectMode,omitempty" json:"ExpectMode,omitempty"` // how to buckets should handle event : types.TIMEMACHINE or types.LIVE
	Whitelisted     bool   `yaml:"Whitelisted,omitempty" json:"Whitelisted,omitempty"`
	WhitelistReason string `yaml:"WhitelistReason,omitempty" json:"whitelist_reason,omitempty"`
	// should add whitelist reason ?
	/* the current stage of the line being parsed */
	Stage string `yaml:"Stage,omitempty" json:"Stage,omitempty"`
	/* original line (produced by acquisition) */
	Line Line `yaml:"Line,omitempty" json:"Line,omitempty"`
	/* output of groks */
	Parsed map[string]string `yaml:"Parsed,omitempty" json:"Parsed,omitempty"`
	/* output of enrichment */
	Enriched map[string]string `yaml:"Enriched,omitempty" json:"Enriched,omitempty"`
	/* output of Unmarshal */
	Unmarshaled map[string]any `yaml:"Unmarshaled,omitempty" json:"Unmarshaled,omitempty"`
	/* Overflow */
	Overflow      RuntimeAlert `yaml:"Overflow,omitempty" json:"Alert,omitempty"`
	Time          time.Time    `yaml:"Time,omitempty" json:"Time,omitempty"` // parsed time `json:"-"` ``
	StrTime       string       `yaml:"StrTime,omitempty" json:"StrTime,omitempty"`
	StrTimeFormat string       `yaml:"StrTimeFormat,omitempty" json:"StrTimeFormat,omitempty"`
	MarshaledTime string       `yaml:"MarshaledTime,omitempty" json:"MarshaledTime,omitempty"`
	Process       bool         `yaml:"Process,omitempty" json:"Process,omitempty"` // can be set to false to avoid processing line
	Appsec        AppsecEvent  `yaml:"Appsec,omitempty" json:"Appsec,omitempty"`
	/* Meta is the only part that will make it to the API - it should be normalized */
	Meta map[string]string `yaml:"Meta,omitempty" json:"Meta,omitempty"`
}

func MakeEvent(timeMachine bool, evtType int, process bool) Event {
	evt := Event{
		Parsed:      make(map[string]string),
		Meta:        make(map[string]string),
		Unmarshaled: make(map[string]any),
		Enriched:    make(map[string]string),
		ExpectMode:  LIVE,
		Process:     process,
		Type:        evtType,
	}
	if timeMachine {
		evt.ExpectMode = TIMEMACHINE
	}

	return evt
}

func (e *Event) SetMeta(key string, value string) bool {
	if e.Meta == nil {
		e.Meta = make(map[string]string)
	}

	e.Meta[key] = value

	return true
}

func (e *Event) SetParsed(key string, value string) bool {
	if e.Parsed == nil {
		e.Parsed = make(map[string]string)
	}

	e.Parsed[key] = value

	return true
}

func (e *Event) GetType() string {
	switch e.Type {
	case OVFLW:
		return "overflow"
	case LOG:
		return "log"
	default:
		log.Warningf("unknown event type for %+v", e)
		return "unknown"
	}
}

func (e *Event) GetMeta(key string) string {
	if e.Type == OVFLW {
		alerts := e.Overflow.APIAlerts
		for idx := range alerts {
			for _, event := range alerts[idx].Events {
				if event.GetMeta(key) != "" {
					return event.GetMeta(key)
				}
			}
		}
	} else if e.Type == LOG {
		for k, v := range e.Meta {
			if k == key {
				return v
			}
		}
	}

	return ""
}

func (e *Event) ParseIPSources() []netip.Addr {
	var srcs []netip.Addr

	switch e.Type {
	case LOG:
		if val, ok := e.Meta["source_ip"]; ok {
			if addr, err := netip.ParseAddr(val); err == nil {
				srcs = append(srcs, addr)
			} else {
				log.Errorf("failed to parse source_ip %s: %v", val, err)
			}
		}
	case OVFLW:
		for k := range e.Overflow.Sources {
			if addr, err := netip.ParseAddr(k); err == nil {
				srcs = append(srcs, addr)
			} else {
				log.Errorf("failed to parse source %s: %v", k, err)
			}
		}
	}

	return srcs
}

// Move in leakybuckets
const (
	Undefined = ""
	Ip        = "Ip"
	Range     = "Range"
	Filter    = "Filter"
	Country   = "Country"
	AS        = "AS"
)

// Move in leakybuckets
type ScopeType struct {
	Scope         string `yaml:"type"`
	Filter        string `yaml:"expression"`
	RunTimeFilter *vm.Program
}

type RuntimeAlert struct {
	Mapkey      string                   `yaml:"MapKey,omitempty" json:"MapKey,omitempty"`
	BucketId    string                   `yaml:"BucketId,omitempty" json:"BucketId,omitempty"`
	Whitelisted bool                     `yaml:"Whitelisted,omitempty" json:"Whitelisted,omitempty"`
	Reprocess   bool                     `yaml:"Reprocess,omitempty" json:"Reprocess,omitempty"`
	Sources     map[string]models.Source `yaml:"Sources,omitempty" json:"Sources,omitempty"`
	Alert       *models.Alert            `yaml:"Alert,omitempty" json:"Alert,omitempty"` // this one is a pointer to APIAlerts[0] for convenience.
	// APIAlerts will be populated at the end when there is more than one source
	APIAlerts []models.Alert `yaml:"APIAlerts,omitempty" json:"APIAlerts,omitempty"`
}

func (r RuntimeAlert) GetSources() []string {
	ret := make([]string, 0)
	for key := range r.Sources {
		ret = append(ret, key)
	}

	return ret
}

func NormalizeScope(scope string) string {
	switch strings.ToLower(scope) {
	case "ip":
		return Ip
	case "range":
		return Range
	case "as":
		return AS
	case "country":
		return Country
	default:
		return scope
	}
}
