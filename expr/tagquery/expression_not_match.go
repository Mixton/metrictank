package tagquery

import (
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type expressionNotMatch struct {
	expressionCommon
	valueRe *regexp.Regexp
}

func (e *expressionNotMatch) GetOperator() ExpressionOperator {
	return NOT_MATCH
}

func (e *expressionNotMatch) RequiresNonEmptyValue() bool {
	return false
}

func (e *expressionNotMatch) HasRe() bool {
	return true
}

func (e *expressionNotMatch) ValuePasses(value string) bool {
	return !e.valueRe.MatchString(value)
}

func (e *expressionNotMatch) GetDefaultDecision() FilterDecision {
	// if the pattern matches "" (f.e. "tag!=~.*) then a metric which
	// does not have the tag "tag" at all should not be part of the
	// result set
	// docs: https://graphite.readthedocs.io/en/latest/tags.html
	// > Any tag spec that matches an empty value is considered to
	// > match series that don’t have that tag
	if e.matchesEmpty {
		return Fail
	}
	return Pass
}

func (e *expressionNotMatch) StringIntoBuilder(builder *strings.Builder) {
	builder.WriteString(e.key)
	builder.WriteString("!=~")
	builder.WriteString(e.value)
}

func (e *expressionNotMatch) GetMetricDefinitionFilter() MetricDefinitionFilter {
	if e.key == "name" {
		if e.value == "" {
			// every metric has a name
			return func(_ string, _ []string) FilterDecision { return Pass }
		}

		return func(name string, _ []string) FilterDecision {
			if e.valueRe.MatchString(name) {
				return Fail
			}
			return Pass
		}
	}

	var matchCache, missCache sync.Map
	var currentMatchCacheSize, currentMissCacheSize int32
	prefix := e.key + "="

	return func(_ string, tags []string) FilterDecision {
		for _, tag := range tags {
			if !strings.HasPrefix(tag, prefix) {
				continue
			}

			// if value is empty, every metric which has this tag passes
			if e.value == "" {
				return Pass
			}

			value := tag[len(prefix):]

			// reduce regex matching by looking up cached non-matches
			if _, ok := missCache.Load(value); ok {
				return Pass
			}

			// reduce regex matching by looking up cached matches
			if _, ok := matchCache.Load(value); ok {
				return Fail
			}

			if e.valueRe.MatchString(value) {
				if atomic.LoadInt32(&currentMatchCacheSize) < int32(matchCacheSize) {
					matchCache.Store(value, struct{}{})
					atomic.AddInt32(&currentMatchCacheSize, 1)
				}
				return Fail
			} else {
				if atomic.LoadInt32(&currentMissCacheSize) < int32(matchCacheSize) {
					missCache.Store(value, struct{}{})
					atomic.AddInt32(&currentMissCacheSize, 1)
				}
				return Pass
			}
		}

		return None
	}
}
