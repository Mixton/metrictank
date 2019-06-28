package tagquery

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const invalidExpressionError = "Invalid expression: %s"

var matchCacheSize int

type Expressions []Expression

func ParseExpressions(expressions []string) (Expressions, error) {
	res := make(Expressions, len(expressions))
	for i := range expressions {
		expression, err := ParseExpression(expressions[i])
		if err != nil {
			return nil, err
		}
		res[i] = expression
	}
	return res, nil
}

// SortByFilterOrder sorts all the expressions first by operator
// roughly in cost-increaseing order when they are used as filters,
// then by key, then by value
func (e Expressions) SortByFilterOrder() {
	costByOperator := map[ExpressionOperator]int{
		MATCH_NONE:  0,
		EQUAL:       1,
		HAS_TAG:     2,
		PREFIX:      3,
		PREFIX_TAG:  4,
		NOT_EQUAL:   5,
		NOT_HAS_TAG: 6,
		MATCH:       7,
		MATCH_TAG:   8,
		NOT_MATCH:   9,
		MATCH_ALL:   10,
	}

	sort.Slice(e, func(i, j int) bool {
		if e[i].GetOperator() == e[j].GetOperator() {
			if e[i].GetKey() == e[j].GetKey() {
				return e[i].GetValue() < e[j].GetValue()
			}
			return e[i].GetKey() < e[j].GetKey()
		}
		return costByOperator[e[i].GetOperator()] < costByOperator[e[j].GetOperator()]
	})
}

// findInitialExpression returns the id of the expression which is the
// most suitable to start the query execution with. the chosen expression
// should be as cheap as possible and it must require a non-empty value
func (e Expressions) findInitialExpression() int {
	// order of preference to start with the viable operators
	for _, op := range []ExpressionOperator{
		EQUAL,
		HAS_TAG,
		PREFIX,
		PREFIX_TAG,
		MATCH,
		MATCH_TAG,
		NOT_MATCH,
	} {
		for i := range e {
			if e[i].GetOperator() == op && e[i].RequiresNonEmptyValue() {
				return i
			}
		}
	}
	return -1
}

func (e Expressions) Strings() []string {
	builder := strings.Builder{}
	res := make([]string, len(e))
	for i := range e {
		e[i].StringIntoBuilder(&builder)
		res[i] = builder.String()
		builder.Reset()
	}
	return res
}

type Expression interface {
	// GetMetricDefinitionFilter returns a MetricDefinitionFilter. It takes a metric definition, looks
	// at its tags and returns a decision regarding this query expression applied to its tags.
	GetMetricDefinitionFilter() MetricDefinitionFilter

	// GetDefaultDecision defines what decision should be made if the filter has not come to a conclusive
	// decision based on a single index. When looking at more than one tag index in order of decreasing
	// priority to decide whether a metric should be part of the final result set, some operators and metric
	// combinations can come to a conclusive decision without looking at all indexes and some others can't.
	// if an expression has evaluated a metric against all indexes and has not come to a conclusive
	// decision, then the default decision gets applied.
	//
	// Example
	// metric1 has tags ["name=a.b.c", "some=value"] in the metric tag index, we evaluate the expression
	// "anothertag!=value":
	// 1) expression looks at the metric tag index and it sees that metric1 does not have a tag "anothertag"
	//    with the value "value", but at this point it doesn't know if another index that will be looked
	//    at later does, so it returns the decision "none".
	// 2) expression now looks at index2 and sees again that metric1 does not have the tag and value
	//    it is looking for, so it returns "none" again.
	// 3) the expression execution sees that there are no more indexes left, so it applies the default
	//    decision for the operator != which is "pass", meaning the expression "anothertag!=value" has
	//    not filtered the metric metric1 out of the result set.
	//
	// metric2 has tags ["name=a.b.c", "anothertag=value"] according to the metric tag index and it has
	// no meta tags, we still evaluate the same expression:
	// 1) expression looks at metric tag index and see metric2 has tag "anothertag" with value "value".
	//    it directly comes to a conclusive decision that this metric needs to be filtered out of the
	//    result set and returns the filter decision "fail".
	//
	// metric3 has tags ["name=aaa", "abc=cba"] according to the metric tag index and there is a meta
	// record assigning the tag "anothertag=value" to metrics matching that query expression "abc=cba".
	// 1) expression looks at metric3 and sees it does not have the tag & value it's looking for, so
	//    it returns the filter decision "none" because it cannot know for sure whether another index
	//    will assign "anothertag=value" to metric3.
	// 2) expression looks at the meta tag index and it sees that there are meta records matching the
	//    tag "anothertag" and the value "value", so it retrieves the according filter functions of
	//    of these meta records and passes metric3's tag set into them.
	// 3) the filter function of the meta record for the query set "abc=cba" returns true, indicating
	//    that its meta tag gets applied to metric3.
	// 4) based on that the tag expression comes to the decision that metric3 should not be part of
	//    final result set, so it returns "fail".
	GetDefaultDecision() FilterDecision

	// GetKey returns tag to who's values this expression get's applied if it operates on the value
	// (OperatorsOnTag returns "false")
	// example:
	// in the expression "tag1=value" GetKey() would return "tag1" and OperatesOnTag() returns "false"
	GetKey() string

	// GetValue the value part of the expression
	// example:
	// in the expression "abc!=cba" this would return "cba"
	GetValue() string

	// GetOperator returns the operator of this expression
	GetOperator() ExpressionOperator

	// FilterValues takes a map that's indexed by strings and applies this expression's criteria to
	// each of the strings, then it returns the strings that have matched
	// In case of expressions that get applied to tags, the first level map of the metric tag index
	// or meta tag index can get passed into this function, otherwise the second level under the key
	// returned by GetKey()
	ValuePasses(string) bool

	// HasRe indicates whether the evaluation of this expression involves regular expressions
	HasRe() bool

	RequiresNonEmptyValue() bool
	OperatesOnTag() bool
	StringIntoBuilder(builder *strings.Builder)
}

// ParseExpression returns an expression that's been generated from the given
// string, in case of an error the error gets returned as the second value
func ParseExpression(expr string) (Expression, error) {
	var pos int
	prefix, regex, not := false, false, false
	resCommon := expressionCommon{}

	// scan up to operator to get key
FIND_OPERATOR:
	for ; pos < len(expr); pos++ {
		switch expr[pos] {
		case '=':
			break FIND_OPERATOR
		case '!':
			not = true
			break FIND_OPERATOR
		case '^':
			prefix = true
			break FIND_OPERATOR
		case ';':
			return nil, fmt.Errorf(invalidExpressionError, expr)
		}
	}

	// key must not be empty
	if pos == 0 {
		return nil, fmt.Errorf(invalidExpressionError, expr)
	}

	resCommon.key = expr[:pos]
	err := validateQueryExpressionTagKey(resCommon.key)
	if err != nil {
		return nil, fmt.Errorf("Error when validating key \"%s\" of expression \"%s\": %s", resCommon.key, expr, err)
	}

	// shift over the !/^ characters
	if not || prefix {
		pos++
	}

	if len(expr) <= pos || expr[pos] != '=' {
		return nil, fmt.Errorf(invalidExpressionError, expr)
	}
	pos++

	if len(expr) > pos && expr[pos] == '~' {
		// ^=~ is not a valid operator
		if prefix {
			return nil, fmt.Errorf(invalidExpressionError, expr)
		}
		regex = true
		pos++
	}

	valuePos := pos
	for ; pos < len(expr); pos++ {
		// disallow ; in value
		if expr[pos] == 59 {
			return nil, fmt.Errorf(invalidExpressionError, expr)
		}
	}
	resCommon.value = expr[valuePos:]
	var operator ExpressionOperator

	if not {
		if len(resCommon.value) == 0 {
			operator = HAS_TAG
		} else if regex {
			operator = NOT_MATCH
		} else {
			operator = NOT_EQUAL
		}
	} else {
		if prefix {
			if len(resCommon.value) == 0 {
				operator = HAS_TAG
			} else {
				operator = PREFIX
			}
		} else if len(resCommon.value) == 0 {
			operator = NOT_HAS_TAG
		} else if regex {
			operator = MATCH
		} else {
			operator = EQUAL
		}
	}

	// special key to match on tag instead of a value
	if resCommon.key == "__tag" {
		// currently ! (not) queries on tags are not supported
		// and unlike normal queries a value must be set
		if not || len(resCommon.value) == 0 {
			return nil, fmt.Errorf(invalidExpressionError, expr)
		}

		if operator == PREFIX {
			operator = PREFIX_TAG
		} else if operator == MATCH {
			operator = MATCH_TAG
		}
	}

	if operator == MATCH || operator == NOT_MATCH || operator == MATCH_TAG {
		if len(resCommon.value) > 0 && resCommon.value[0] != '^' {
			resCommon.value = "^(?:" + resCommon.value + ")"
		}

		valueRe, err := regexp.Compile(resCommon.value)
		if err != nil {
			return nil, err
		}
		switch operator {
		case MATCH:
			return &expressionMatch{expressionCommon: resCommon, valueRe: valueRe}, nil
		case NOT_MATCH:
			return &expressionNotMatch{expressionCommon: resCommon, valueRe: valueRe}, nil
		case MATCH_TAG:
			return &expressionMatchTag{expressionCommon: resCommon, valueRe: valueRe}, nil
		}
	} else {
		switch operator {
		case EQUAL:
			return &expressionEqual{expressionCommon: resCommon}, nil
		case NOT_EQUAL:
			return &expressionNotEqual{expressionCommon: resCommon}, nil
		case PREFIX:
			return &expressionPrefix{expressionCommon: resCommon}, nil
		case MATCH_TAG:
			return &expressionMatchTag{expressionCommon: resCommon}, nil
		case HAS_TAG:
			return &expressionHasTag{expressionCommon: resCommon}, nil
		case NOT_HAS_TAG:
			return &expressionNotHasTag{expressionCommon: resCommon}, nil
		case PREFIX_TAG:
			return &expressionPrefixTag{expressionCommon: resCommon}, nil
		}
	}

	return nil, fmt.Errorf("ParseExpression: Invalid operator in expression %s", expr)
}

func ExpressionsAreEqual(expr1, expr2 Expression) bool {
	return expr1.GetKey() == expr2.GetKey() && expr1.GetOperator() == expr2.GetOperator() && expr1.GetValue() == expr2.GetValue()
}

// MetricDefinitionFilter takes a metric name together with its tags and returns a FilterDecision
type MetricDefinitionFilter func(name string, tags []string) FilterDecision

type MetricDefinitionFilters []MetricDefinitionFilter

func (m MetricDefinitionFilters) Filter(name string, tags []string) FilterDecision {
	for i := range m {
		decision := m[i](name, tags)
		if decision == Fail {
			return Fail
		} else if decision == Pass {
			return Pass
		}
	}

	return None
}

type FilterDecision uint8

const (
	None FilterDecision = iota // no decision has been made, because the decision might change depending on what other indexes defines
	Fail                       // it has been decided by the filter that this metric does not end up in the result set
	Pass                       // the filter has passed
)

type ExpressionOperator uint16

const (
	EQUAL       ExpressionOperator = iota // =
	NOT_EQUAL                             // !=
	MATCH                                 // =~        regular expression
	MATCH_TAG                             // __tag=~   relies on special key __tag. non-standard, required for `/metrics/tags` requests with "filter"
	NOT_MATCH                             // !=~
	PREFIX                                // ^=        exact prefix, not regex. non-standard, required for auto complete of tag values
	PREFIX_TAG                            // __tag^=   exact prefix with tag. non-standard, required for auto complete of tag keys
	HAS_TAG                               // <tag>!="" specified tag must be present
	NOT_HAS_TAG                           // <tag>="" specified tag must not be present
)

func (o ExpressionOperator) StringIntoBuilder(builder *strings.Builder) {
	switch o {
	case EQUAL:
		builder.WriteString("=")
	case NOT_EQUAL:
		builder.WriteString("!=")
	case MATCH:
		builder.WriteString("=~")
	case MATCH_TAG:
		builder.WriteString("=~")
	case NOT_MATCH:
		builder.WriteString("!=~")
	case PREFIX:
		builder.WriteString("^=")
	case PREFIX_TAG:
		builder.WriteString("^=")
	case HAS_TAG:
		builder.WriteString("!=")
	case NOT_HAS_TAG:
		builder.WriteString("=")
	}
}
