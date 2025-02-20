package jfr

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/cespare/xxhash"
	"github.com/pyroscope-io/jfr-parser/parser"
	"github.com/pyroscope-io/pyroscope/pkg/storage/segment"
	"github.com/pyroscope-io/pyroscope/pkg/storage/tree"

	"github.com/alibaba/ilogtail/helper/profile"
	"github.com/alibaba/ilogtail/pkg/logger"
)

const (
	_ = iota
	sampleTypeCPU
	sampleTypeWall
	sampleTypeInTLABObjects
	sampleTypeInTLABBytes
	sampleTypeOutTLABObjects
	sampleTypeOutTLABBytes
	sampleTypeLockSamples
	sampleTypeLockDuration
)

func (r *RawProfile) ParseJFR(ctx context.Context, meta *profile.Meta, body io.Reader, jfrLabels *LabelsSnapshot, cb profile.CallbackFunc) (err error) {
	if meta.SampleRate > 0 {
		meta.Tags["_sample_rate_"] = strconv.FormatUint(uint64(meta.SampleRate), 10)
	}
	chunks, err := parser.ParseWithOptions(body, &parser.ChunkParseOptions{
		CPoolProcessor: processSymbols,
	})
	if err != nil {
		return fmt.Errorf("unable to parse JFR format: %w", err)
	}
	for _, c := range chunks {
		r.parseChunk(ctx, meta, c, jfrLabels, cb)
	}
	return nil
}

// revive:disable-next-line:cognitive-complexity necessary complexity
func (r *RawProfile) parseChunk(ctx context.Context, meta *profile.Meta, c parser.Chunk, jfrLabels *LabelsSnapshot, convertCb profile.CallbackFunc) {
	stackMap := make(map[uint64]*profile.Stack)
	valMap := make(map[uint64][]uint64)
	labelMap := make(map[uint64]map[string]string)
	typeMap := make(map[uint64][]string)
	unitMap := make(map[uint64][]string)
	aggtypeMap := make(map[uint64][]string)

	var event string
	for _, e := range c.Events {
		if as, ok := e.(*parser.ActiveSetting); ok {
			if as.Name == "event" {
				event = as.Value
			}
		}
	}
	cache := make(tree.LabelsCache)
	for contextID, events := range groupEventsByContextID(c.Events) {
		labels := getContextLabels(contextID, jfrLabels)
		lh := labels.Hash()
		for _, e := range events {
			switch obj := e.(type) {
			case *parser.ExecutionSample:
				if fs := frames(obj.StackTrace); fs != nil {
					if obj.State.Name == "STATE_RUNNABLE" {
						cache.GetOrCreateTreeByHash(sampleTypeCPU, labels, lh).InsertStackString(fs, 1)
					}
					cache.GetOrCreateTreeByHash(sampleTypeWall, labels, lh).InsertStackString(fs, 1)
				}
			case *parser.ObjectAllocationInNewTLAB:
				if fs := frames(obj.StackTrace); fs != nil {
					cache.GetOrCreateTreeByHash(sampleTypeInTLABObjects, labels, lh).InsertStackString(fs, 1)
					cache.GetOrCreateTreeByHash(sampleTypeInTLABBytes, labels, lh).InsertStackString(fs, uint64(obj.TLABSize))
				}
			case *parser.ObjectAllocationOutsideTLAB:
				if fs := frames(obj.StackTrace); fs != nil {
					cache.GetOrCreateTreeByHash(sampleTypeOutTLABObjects, labels, lh).InsertStackString(fs, 1)
					cache.GetOrCreateTreeByHash(sampleTypeOutTLABBytes, labels, lh).InsertStackString(fs, uint64(obj.AllocationSize))
				}
			case *parser.JavaMonitorEnter:
				if fs := frames(obj.StackTrace); fs != nil {
					cache.GetOrCreateTreeByHash(sampleTypeLockSamples, labels, lh).InsertStackString(fs, 1)
					cache.GetOrCreateTreeByHash(sampleTypeLockDuration, labels, lh).InsertStackString(fs, uint64(obj.Duration))
				}
			case *parser.ThreadPark:
				if fs := frames(obj.StackTrace); fs != nil {
					cache.GetOrCreateTreeByHash(sampleTypeLockSamples, labels, lh).InsertStackString(fs, 1)
					cache.GetOrCreateTreeByHash(sampleTypeLockDuration, labels, lh).InsertStackString(fs, uint64(obj.Duration))
				}
			}
		}
	}
	for sampleType, entries := range cache {
		for _, e := range entries {
			if i := labelIndex(jfrLabels, e.Labels, segment.ProfileIDLabelName); i != -1 {
				cutLabels := tree.CutLabel(e.Labels, i)
				cache.GetOrCreateTree(sampleType, cutLabels).Merge(e.Tree)
			}
		}
	}
	cb := func(n string, labels tree.Labels, t *tree.Tree, u profile.Units) {
		t.IterateStacks(func(name string, self uint64, stack []string) {
			id := xxhash.Sum64String(strings.Join(stack, ""))
			stackMap[id] = &profile.Stack{
				Name:  profile.FormatPositionAndName(name, profile.FormatType(meta.SpyName)),
				Stack: profile.FormatPostionAndNames(stack[1:], profile.FormatType(meta.SpyName)),
			}
			aggtypeMap[id] = append(aggtypeMap[id], string(meta.AggregationType))
			typeMap[id] = append(typeMap[id], n)
			unitMap[id] = append(unitMap[id], string(u))
			valMap[id] = append(valMap[id], self)
			labelMap[id] = buildKey(meta.Tags, labels, jfrLabels).Labels()
		})
	}
	for sampleType, entries := range cache {
		if sampleType == sampleTypeWall && event != "wall" {
			continue
		}
		n := getName(sampleType, event)
		units := getUnits(sampleType)
		for _, e := range entries {
			cb(n, e.Labels, e.Tree, units)
		}
	}

	for id, fs := range stackMap {
		if len(valMap[id]) == 0 || len(typeMap[id]) == 0 || len(unitMap[id]) == 0 || len(aggtypeMap[id]) == 0 || len(labelMap[id]) == 0 {
			logger.Warning(ctx, "PPROF_PROFILE_ALARM", "stack don't have enough meta or values", fs)
			continue
		}
		convertCb(id, fs, valMap[id], typeMap[id], unitMap[id], aggtypeMap[id], meta.StartTime.UnixNano(), meta.EndTime.UnixNano(), labelMap[id])
	}
}

func getName(sampleType int64, event string) string {
	switch sampleType {
	case sampleTypeCPU:
		if event == "cpu" || event == "itimer" || event == "wall" {
			profile := event
			if event == "wall" {
				profile = "cpu"
			}
			return profile
		}
	case sampleTypeWall:
		return "wall"
	case sampleTypeInTLABObjects:
		return "alloc_in_new_tlab_objects"
	case sampleTypeInTLABBytes:
		return "alloc_in_new_tlab_bytes"
	case sampleTypeOutTLABObjects:
		return "alloc_outside_tlab_objects"
	case sampleTypeOutTLABBytes:
		return "alloc_outside_tlab_bytes"
	case sampleTypeLockSamples:
		return "lock_count"
	case sampleTypeLockDuration:
		return "lock_duration"
	}
	return "unknown"
}

func getUnits(sampleType int64) profile.Units {
	switch sampleType {
	case sampleTypeCPU:
		return profile.SamplesUnits
	case sampleTypeWall:
		return profile.SamplesUnits
	case sampleTypeInTLABObjects:
		return profile.ObjectsUnit
	case sampleTypeInTLABBytes:
		return profile.BytesUnit
	case sampleTypeOutTLABObjects:
		return profile.ObjectsUnit
	case sampleTypeOutTLABBytes:
		return profile.BytesUnit
	case sampleTypeLockSamples:
		return profile.LockSamplesUnits
	case sampleTypeLockDuration:
		return profile.LockNanosecondsUnits
	}
	return profile.SamplesUnits
}

func buildKey(appLabels map[string]string, labels tree.Labels, snapshot *LabelsSnapshot) *segment.Key {
	finalLabels := map[string]string{}
	for k, v := range appLabels {
		finalLabels[k] = v
	}
	for _, v := range labels {
		ks, ok := snapshot.Strings[v.Key]
		if !ok {
			continue
		}
		vs, ok := snapshot.Strings[v.Str]
		if !ok {
			continue
		}
		finalLabels[ks] = vs
	}
	return segment.NewKey(finalLabels)
}

func getContextLabels(contextID int64, labels *LabelsSnapshot) tree.Labels {
	if contextID == 0 {
		return nil
	}
	var ctx *Context
	var ok bool
	if ctx, ok = labels.Contexts[contextID]; !ok {
		return nil
	}
	res := make(tree.Labels, 0, len(ctx.Labels))
	for k, v := range ctx.Labels {
		res = append(res, &tree.Label{Key: k, Str: v})
	}
	return res
}
func labelIndex(s *LabelsSnapshot, labels tree.Labels, key string) int {
	for i, label := range labels {
		if n, ok := s.Strings[label.Key]; ok {
			if n == key {
				return i
			}
		}
	}
	return -1
}

func groupEventsByContextID(events []parser.Parseable) map[int64][]parser.Parseable {
	res := make(map[int64][]parser.Parseable)
	for _, e := range events {
		switch obj := e.(type) {
		case *parser.ExecutionSample:
			res[obj.ContextId] = append(res[obj.ContextId], e)
		case *parser.ObjectAllocationInNewTLAB:
			res[obj.ContextId] = append(res[obj.ContextId], e)
		case *parser.ObjectAllocationOutsideTLAB:
			res[obj.ContextId] = append(res[obj.ContextId], e)
		case *parser.JavaMonitorEnter:
			res[obj.ContextId] = append(res[obj.ContextId], e)
		case *parser.ThreadPark:
			res[obj.ContextId] = append(res[obj.ContextId], e)
		}
	}
	return res
}

func frames(st *parser.StackTrace) []string {
	if st == nil {
		return nil
	}
	frames := make([]string, 0, len(st.Frames))
	for i := len(st.Frames) - 1; i >= 0; i-- {
		f := st.Frames[i]
		// TODO(abeaumont): Add support for line numbers.
		if f.Method != nil && f.Method.Type != nil && f.Method.Type.Name != nil && f.Method.Name != nil {
			frames = append(frames, f.Method.Type.Name.String+"."+f.Method.Name.String)
		}
	}
	return frames
}

// jdk/internal/reflect/GeneratedMethodAccessor31
var generatedMethodAccessor = regexp.MustCompile(`^(jdk/internal/reflect/GeneratedMethodAccessor)(\d+)$`)

// org/example/rideshare/OrderService$$Lambda$669.0x0000000800fd7318.run
var lambdaGeneratedEnclosingClass = regexp.MustCompile(`^(.+\$\$Lambda\$)\d+[./](0x[\da-f]+|\d+)$`)

// libzstd-jni-1.5.1-16931311898282279136.so.Java_com_github_luben_zstd_ZstdInputStreamNoFinalizer_decompressStream
var zstdJniSoLibName = regexp.MustCompile(`^(\.?/tmp/)?(libzstd-jni-\d+\.\d+\.\d+-)(\d+)(\.so)( \(deleted\))?$`)

// ./tmp/libamazonCorrettoCryptoProvider109b39cf33c563eb.so
var amazonCorrettoCryptoProvider = regexp.MustCompile(`^(\.?/tmp/)?(libamazonCorrettoCryptoProvider)([0-9a-f]{16})(\.so)( \(deleted\))?$`)

// libasyncProfiler-linux-arm64-17b9a1d8156277a98ccc871afa9a8f69215f92.so
var pyroscopeAsyncProfiler = regexp.MustCompile(
	`^(\.?/tmp/)?(libasyncProfiler)-(linux-arm64|linux-musl-x64|linux-x64|macos)-(17b9a1d8156277a98ccc871afa9a8f69215f92)(\.so)( \(deleted\))?$`)

func mergeJVMGeneratedClasses(frame string) string {
	frame = generatedMethodAccessor.ReplaceAllString(frame, "${1}_")
	frame = lambdaGeneratedEnclosingClass.ReplaceAllString(frame, "${1}_")
	frame = zstdJniSoLibName.ReplaceAllString(frame, "libzstd-jni-_.so")
	frame = amazonCorrettoCryptoProvider.ReplaceAllString(frame, "libamazonCorrettoCryptoProvider_.so")
	frame = pyroscopeAsyncProfiler.ReplaceAllString(frame, "libasyncProfiler-_.so")
	return frame
}

func processSymbols(meta parser.ClassMetadata, cpool *parser.CPool) {
	if meta.Name == "jdk.types.Symbol" {
		for _, v := range cpool.Pool {
			sym := v.(*parser.Symbol)
			sym.String = mergeJVMGeneratedClasses(sym.String)
		}
	}
}
