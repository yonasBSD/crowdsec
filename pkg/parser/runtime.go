package parser

/*
 This file contains
 - the runtime parsing routines
*/

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mohae/deepcopy"
	log "github.com/sirupsen/logrus"

	"github.com/crowdsecurity/crowdsec/pkg/dumps"
	"github.com/crowdsecurity/crowdsec/pkg/exprhelpers"
	"github.com/crowdsecurity/crowdsec/pkg/types"
)

/* ok, this is kinda experimental, I don't know how bad of an idea it is .. */
func SetTargetByName(target string, value string, evt *types.Event) bool {
	if evt == nil {
		return false
	}

	// it's a hack, we do it for the user
	target = strings.TrimPrefix(target, "evt.")

	log.Debugf("setting target %s to %s", target, value)

	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Runtime error while trying to set '%s': %+v", target, r)
			return
		}
	}()

	iter := reflect.ValueOf(evt).Elem()
	if !iter.IsValid() || iter.IsZero() {
		log.Tracef("event is nil")
		return false
	}

	for f := range strings.SplitSeq(target, ".") {
		/*
		** According to current Event layout we only have to handle struct and map
		 */
		switch iter.Kind() { //nolint:exhaustive
		case reflect.Map:
			tmp := iter.MapIndex(reflect.ValueOf(f))
			/*if we're in a map and the field doesn't exist, the user wants to add it :) */
			if !tmp.IsValid() || tmp.IsZero() {
				log.Debugf("map entry is zero in '%s'", target)
			}

			iter.SetMapIndex(reflect.ValueOf(f), reflect.ValueOf(value))

			return true
		case reflect.Struct:
			tmp := iter.FieldByName(f)
			if !tmp.IsValid() {
				log.Debugf("'%s' is not a valid target because '%s' is not valid", target, f)
				return false
			}

			if tmp.Kind() == reflect.Ptr {
				tmp = reflect.Indirect(tmp)
			}

			iter = tmp
		case reflect.Ptr:
			tmp := iter.Elem()
			iter = reflect.Indirect(tmp.FieldByName(f))
		default:
			log.Errorf("unexpected type %s in '%s'", iter.Kind(), target)
			return false
		}
	}

	// now we should have the final member :)
	if !iter.CanSet() {
		log.Errorf("'%s' can't be set", target)
		return false
	}

	if iter.Kind() != reflect.String {
		log.Errorf("Expected string, got %v when handling '%s'", iter.Kind(), target)
		return false
	}

	iter.Set(reflect.ValueOf(value))

	return true
}

func printStaticTarget(static ExtraField) string {
	switch {
	case static.Method != "":
		return static.Method
	case static.Parsed != "":
		return fmt.Sprintf(".Parsed[%s]", static.Parsed)
	case static.Meta != "":
		return fmt.Sprintf(".Meta[%s]", static.Meta)
	case static.Enriched != "":
		return fmt.Sprintf(".Enriched[%s]", static.Enriched)
	case static.TargetByName != "":
		return static.TargetByName
	default:
		return "?"
	}
}

func (n *Node) ProcessStatics(statics []ExtraField, event *types.Event) error {
	//we have a few cases :
	//(meta||key) + (static||reference||expr)
	var value string

	clog := n.Logger

	for _, static := range statics {
		value = ""
		if static.Value != "" {
			value = static.Value
		} else if static.RunTimeValue != nil {
			output, err := exprhelpers.Run(static.RunTimeValue, map[string]any{"evt": event}, clog, n.Debug)
			if err != nil {
				clog.Warningf("failed to run RunTimeValue : %v", err)
				continue
			}

			switch out := output.(type) {
			case string:
				value = out
			case int:
				value = strconv.Itoa(out)
			case float64, float32:
				value = fmt.Sprintf("%f", out)
			case map[string]any:
				clog.Warnf("Expression '%s' returned a map, please use ToJsonString() to convert it to string if you want to keep it as is, or refine your expression to extract a string", static.ExpValue)
			case []any:
				clog.Warnf("Expression '%s' returned an array, please use ToJsonString() to convert it to string if you want to keep it as is, or refine your expression to extract a string", static.ExpValue)
			case nil:
				clog.Debugf("Expression '%s' returned nil, skipping", static.ExpValue)
			default:
				clog.Errorf("unexpected return type for '%s' : %T", static.ExpValue, output)
				return errors.New("unexpected return type for RunTimeValue")
			}
		}

		if value == "" {
			// allow ParseDate to have empty input
			if static.Method != "ParseDate" {
				clog.Debugf("Empty value for %s, skip.", printStaticTarget(static))
				continue
			}
		}

		if static.Method != "" {
			processed := false
			/*still way too hackish, but : inject all the results in enriched, and */
			if enricherPlugin, ok := n.EnrichFunctions.Registered[static.Method]; ok {
				clog.Tracef("Found method '%s'", static.Method)
				ret, err := enricherPlugin.EnrichFunc(value, event, n.Logger.WithField("method", static.Method))
				if err != nil {
					clog.Errorf("method '%s' returned an error : %v", static.Method, err)
				}
				processed = true
				clog.Debugf("+ Method %s('%s') returned %d entries to merge in .Enriched\n", static.Method, value, len(ret))
				//Hackish check, but those methods do not return any data by design
				if len(ret) == 0 && static.Method != "UnmarshalJSON" {
					clog.Debugf("+ Method '%s' empty response on '%s'", static.Method, value)
				}
				for k, v := range ret {
					clog.Debugf("\t.Enriched[%s] = '%s'\n", k, v)
					event.Enriched[k] = v
				}
			} else {
				clog.Debugf("method '%s' doesn't exist or plugin not initialized", static.Method)
			}

			if !processed {
				clog.Debugf("method '%s' doesn't exist", static.Method)
			}
		} else if static.Parsed != "" {
			clog.Debugf(".Parsed[%s] = '%s'", static.Parsed, value)
			event.Parsed[static.Parsed] = value
		} else if static.Meta != "" {
			clog.Debugf(".Meta[%s] = '%s'", static.Meta, value)
			event.Meta[static.Meta] = value
		} else if static.Enriched != "" {
			clog.Debugf(".Enriched[%s] = '%s'", static.Enriched, value)
			event.Enriched[static.Enriched] = value
		} else if static.TargetByName != "" {
			if !SetTargetByName(static.TargetByName, value, event) {
				clog.Errorf("Unable to set value of '%s'", static.TargetByName)
			} else {
				clog.Debugf("%s = '%s'", static.TargetByName, value)
			}
		} else {
			clog.Fatal("unable to process static : unknown target")
		}
	}

	return nil
}

func stageidx(stage string, stages []string) int {
	for i, v := range stages {
		if stage == v {
			return i
		}
	}

	return -1
}

var (
	ParseDump  bool
	DumpFolder string
)

var (
	StageParseCache dumps.ParserResults
	StageParseMutex sync.Mutex
)

func Parse(ctx UnixParserCtx, xp types.Event, nodes []Node) (types.Event, error) {
	event := xp

	/* the stage is undefined, probably line is freshly acquired, set to first stage !*/
	if event.Stage == "" && len(ctx.Stages) > 0 {
		event.Stage = ctx.Stages[0]
		log.Tracef("no stage, set to : %s", event.Stage)
	}
	event.Process = false
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}

	if event.Parsed == nil {
		event.Parsed = make(map[string]string)
	}
	if event.Enriched == nil {
		event.Enriched = make(map[string]string)
	}
	if event.Meta == nil {
		event.Meta = make(map[string]string)
	}
	if event.Unmarshaled == nil {
		event.Unmarshaled = make(map[string]any)
	}
	if event.Type == types.LOG {
		log.Tracef("INPUT '%s'", event.Line.Raw)
	}

	if ParseDump {
		if StageParseCache == nil {
			StageParseMutex.Lock()
			StageParseCache = make(dumps.ParserResults)
			StageParseCache["success"] = make(map[string][]dumps.ParserResult)
			StageParseCache["success"][""] = make([]dumps.ParserResult, 0)
			StageParseMutex.Unlock()
		}
	}

	for _, stage := range ctx.Stages {
		if ParseDump {
			StageParseMutex.Lock()
			if _, ok := StageParseCache[stage]; !ok {
				StageParseCache[stage] = make(map[string][]dumps.ParserResult)
			}
			StageParseMutex.Unlock()
		}
		/* if the node is forward in stages, seek to this stage */
		/* this is for example used by testing system to inject logs in post-syslog-parsing phase*/
		if stageidx(event.Stage, ctx.Stages) > stageidx(stage, ctx.Stages) {
			log.Tracef("skipping stage, we are already at [%s] expecting [%s]", event.Stage, stage)
			continue
		}
		log.Tracef("node stage : %s, current stage : %s", event.Stage, stage)

		/* if the stage is wrong, it means that the log didn't manage "pass" a stage with a onsuccess: next_stage tag */
		if event.Stage != stage {
			log.Debugf("Event not parsed, expected stage '%s' got '%s', abort", stage, event.Stage)
			event.Process = false
			return event, nil
		}

		isStageOK := false
		for idx := range nodes {
			//Only process current stage's nodes
			if event.Stage != nodes[idx].Stage {
				continue
			}
			clog := log.WithFields(log.Fields{
				"node-name": nodes[idx].rn,
				"stage":     event.Stage,
			})
			clog.Tracef("Processing node %d/%d -> %s", idx, len(nodes), nodes[idx].rn)
			if ctx.Profiling {
				nodes[idx].Profiling = true
			}
			ret, err := nodes[idx].process(&event, ctx, map[string]any{"evt": &event})
			if err != nil {
				clog.Errorf("Error while processing node : %v", err)
				return event, err
			}
			clog.Tracef("node (%s) ret : %v", nodes[idx].rn, ret)
			if ParseDump {
				var parserIdxInStage int
				StageParseMutex.Lock()
				if len(StageParseCache[stage][nodes[idx].Name]) == 0 {
					StageParseCache[stage][nodes[idx].Name] = make([]dumps.ParserResult, 0)
					parserIdxInStage = len(StageParseCache[stage])
				} else {
					parserIdxInStage = StageParseCache[stage][nodes[idx].Name][0].Idx
				}
				StageParseMutex.Unlock()

				evtcopy := deepcopy.Copy(event)
				parserInfo := dumps.ParserResult{Evt: evtcopy.(types.Event), Success: ret, Idx: parserIdxInStage}
				StageParseMutex.Lock()
				StageParseCache[stage][nodes[idx].Name] = append(StageParseCache[stage][nodes[idx].Name], parserInfo)
				StageParseMutex.Unlock()
			}
			if ret {
				isStageOK = true
			}
			if ret && nodes[idx].OnSuccess == "next_stage" {
				clog.Debugf("node successful, stop end stage %s", stage)
				break
			}
			//the parsed object moved onto the next phase
			if event.Stage != stage {
				clog.Tracef("node moved stage, break and redo")
				break
			}
		}
		if !isStageOK {
			log.Debugf("Log didn't finish stage %s", event.Stage)
			event.Process = false
			return event, nil
		}
	}

	event.Process = true
	return event, nil
}
