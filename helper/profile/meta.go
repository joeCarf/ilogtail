package profile

import (
	"context"
	"strings"
	"time"

	"github.com/alibaba/ilogtail/pkg/protocol"

	"github.com/gofrs/uuid"
)

type Input struct {
	Profile  RawProfile
	Metadata Meta
}

type Stack struct {
	Name  string
	Stack []string
}

type Kind int

const (
	_ Kind = iota
	CPUKind
	MemKind
	MutexKind
	GoRoutinesKind
	ExceptionKind
	UnknownKind
)

func (p Kind) String() string {
	switch p {
	case CPUKind:
		return "profile_cpu"
	case MemKind:
		return "profile_mem"
	case MutexKind:
		return "profile_mutex"
	case GoRoutinesKind:
		return "profile_goroutines"
	case ExceptionKind:
		return "profile_exception"
	default:
		return "profile_unknown"
	}
}

type Format string

type CallbackFunc func(id uint64, stack *Stack, vals []uint64, types, units, aggs []string, startTime, endTime int64, labels map[string]string)

const (
	FormatPprof      Format = "pprof"
	FormatJFR        Format = "jfr"
	FormatTrie       Format = "trie"
	FormatTree       Format = "tree"
	FormatLines      Format = "lines"
	FormatGroups     Format = "groups"
	FormatSpeedscope Format = "speedscope"
)

type Meta struct {
	StartTime       time.Time
	EndTime         time.Time
	Tags            map[string]string
	SpyName         string
	SampleRate      uint32
	Units           Units
	AggregationType AggType
}

type AggType string

const (
	AvgAggType AggType = "avg"
	SumAggType AggType = "sum"
)

type Units string

const (
	SamplesUnits         Units = "samples"
	NanosecondsUnit      Units = "nanoseconds"
	ObjectsUnit          Units = "objects"
	BytesUnit            Units = "bytes"
	GoroutinesUnits      Units = "goroutines"
	LockNanosecondsUnits Units = "lock_nanoseconds"
	LockSamplesUnits     Units = "local_samples"
)

func DetectProfileType(valType string) Kind {
	switch valType {
	case "inuse_space", "inuse_objects", "alloc_space", "alloc_objects", "alloc-size", "alloc-samples", "alloc_in_new_tlab_objects", "alloc_in_new_tlab_bytes", "alloc_outside_tlab_objects", "alloc_outside_tlab_bytes":
		return MemKind
	case "samples", "cpu", "itimer", "lock_count", "lock_duration", "wall":
		return CPUKind
	case "mutex_count", "mutex_duration", "block_duration", "block_count", "contentions", "delay", "lock-time", "lock-count":
		return MemKind
	case "goroutines", "goroutine":
		return GoRoutinesKind
	case "exception":
		return ExceptionKind
	default:
		return UnknownKind
	}
}

func GetProfileID(meta *Meta) string {
	var profileIDStr string
	if id, ok := meta.Tags["profile_id"]; ok {
		profileIDStr = id
	} else {
		profileID, _ := uuid.NewV4()
		profileIDStr = profileID.String()
	}
	return profileIDStr
}

type FormatType string

// SequenceMapping demo
// nodejs: ./node_modules/express/lib/router/index.js:process_params:338 /app/node_modules/express/lib/router/index.js
// golang: compress/flate.NewWriter /usr/local/go/src/compress/flate/deflate.go
// rust: backtrace_rs.rs:23 - <pprof::backtrace::backtrace_rs::Trace as pprof::backtrace::Trace>::trace
// dotnet: System.Threading.Tasks!Task.InternalWaitCore System.Private.CoreLib
// ruby: /usr/local/bundle/gems/pyroscope-0.3.0-x86_64-linux/lib/pyroscope.rb:63 - tag_wrapper
// python: lib/utility/utility.py:38 - find_nearest_vehicle
// java: libjvm.so.AdvancedThresholdPolicy::method_back_branch_event
// ebpf: /usr/lib/systemd/systemd+0x93242
// php: <internal> - sleep
var sequenceMapping = map[FormatType]SequenceType{
	PyroscopeNodeJs: FunctionFirst,
	PyroscopeGolang: FunctionFirst,
	PyroscopeRust:   PosFirst,
	PyroscopeDotnet: FunctionFirst,
	PyroscopeRuby:   PosFirst,
	PyroscopePython: PosFirst,
	PyroscopeJava:   FunctionFirst,
	PyroscopeEbpf:   FunctionFirst,
	PyroscopePhp:    PosFirst,
	Unknown:         FunctionFirst,
}

const (
	PyroscopeNodeJs = "node"
	PyroscopeGolang = "go"
	PyroscopeRust   = "rs"
	PyroscopeDotnet = "dotnet"
	PyroscopeRuby   = "rb"
	PyroscopePython = "py"
	PyroscopeJava   = "java"
	PyroscopeEbpf   = "ebpf"
	PyroscopePhp    = "php"
	Unknown         = "unknown"
)

type SequenceType int

const (
	_ SequenceType = iota
	PosFirst
	FunctionFirst
)

func FormatPositionAndName(str string, t FormatType) string {
	str = strings.TrimSpace(str)
	idx := strings.Index(str, " ")
	if idx < 0 {
		return str // means no position
	}
	joiner := func(name, pos string) string {
		var b strings.Builder
		b.Grow(len(name) + len(pos) + 1)
		b.Write([]byte(name))
		b.Write([]byte{' '})
		b.Write([]byte(pos))
		return b.String()
	}
	name := str[:idx]
	idx = strings.LastIndex(str, " ")
	pos := str[idx+1:]
	sequenceType := sequenceMapping[t]
	switch sequenceType {
	case PosFirst:
		return joiner(pos, name)
	case FunctionFirst:
		return joiner(name, pos)
	default:
		return str
	}
}

func FormatPostionAndNames(strs []string, t FormatType) []string {
	for i := range strs {
		strs[i] = FormatPositionAndName(strs[i], t)
	}
	return strs
}

func (u Units) DetectValueType() string {
	switch u {
	case NanosecondsUnit, SamplesUnits:
		return "cpu"
	case ObjectsUnit, BytesUnit:
		return "mem"
	case GoroutinesUnits:
		return "goroutines"
	case LockSamplesUnits, LockNanosecondsUnits:
		return "mutex"
	}
	return "unknown"
}

type RawProfile interface {
	Parse(ctx context.Context, meta *Meta, tags map[string]string) (logs []*protocol.Log, err error)
}
