package expression

import (
	"fmt"
	"sync"

	errors "gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/go-mysql-server/internal/regex"
	"github.com/dolthub/go-mysql-server/sql"
)

var ErrInvalidRegexp = errors.NewKind("Invalid regular expression: %s")

// Comparer implements a comparison expression.
type Comparer interface {
	sql.Expression
	Compare(ctx *sql.Context, row sql.Row) (int, error)
	Left() sql.Expression
	Right() sql.Expression
}

// ErrNilOperand ir returned if some or both of the comparison's operands is nil.
var ErrNilOperand = errors.NewKind("nil operand found in comparison")

type comparison struct {
	BinaryExpression
	compareType sql.Type
}

func newComparison(left, right sql.Expression) comparison {
	return comparison{BinaryExpression{left, right}, nil}
}

// Compare the two given values using the types of the expressions in the comparison.
// Since both types should be equal, it does not matter which type is used, but for
// reference, the left type is always used.
func (c *comparison) Compare(ctx *sql.Context, row sql.Row) (int, error) {
	left, right, err := c.evalLeftAndRight(ctx, row)
	if err != nil {
		return 0, err
	}

	if left == nil || right == nil {
		return 0, ErrNilOperand.New()
	}

	if c.Left().Type() == c.Right().Type() {
		return c.Left().Type().Compare(left, right)
	}

	left, right, err = c.castLeftAndRight(left, right)
	if err != nil {
		return 0, err
	}

	return c.compareType.Compare(left, right)
}

func (c *comparison) evalLeftAndRight(ctx *sql.Context, row sql.Row) (interface{}, interface{}, error) {
	left, err := c.Left().Eval(ctx, row)
	if err != nil {
		return nil, nil, err
	}

	right, err := c.Right().Eval(ctx, row)
	if err != nil {
		return nil, nil, err
	}

	return left, right, nil
}

func (c *comparison) castLeftAndRight(left, right interface{}) (interface{}, interface{}, error) {
	leftType := c.Left().Type()
	rightType := c.Right().Type()
	if sql.IsNumber(leftType) || sql.IsNumber(rightType) {
		if sql.IsDecimal(leftType) || sql.IsDecimal(rightType) {
			//TODO: We need to set to the actual DECIMAL type
			l, r, err := convertLeftAndRight(left, right, ConvertToDecimal)
			if err != nil {
				return nil, nil, err
			}

			if sql.IsDecimal(leftType) {
				c.compareType = leftType
			} else {
				c.compareType = rightType
			}
			return l, r, nil
		}

		if sql.IsFloat(leftType) || sql.IsFloat(rightType) {
			l, r, err := convertLeftAndRight(left, right, ConvertToDouble)
			if err != nil {
				return nil, nil, err
			}

			c.compareType = sql.Float64
			return l, r, nil
		}

		if sql.IsSigned(leftType) || sql.IsSigned(rightType) {
			l, r, err := convertLeftAndRight(left, right, ConvertToSigned)
			if err != nil {
				return nil, nil, err
			}

			c.compareType = sql.Int64
			return l, r, nil
		}

		l, r, err := convertLeftAndRight(left, right, ConvertToUnsigned)
		if err != nil {
			return nil, nil, err
		}

		c.compareType = sql.Uint64
		return l, r, nil
	}

	left, right, err := convertLeftAndRight(left, right, ConvertToChar)
	if err != nil {
		return nil, nil, err
	}

	c.compareType = sql.LongText
	return left, right, nil
}

func convertLeftAndRight(left, right interface{}, convertTo string) (interface{}, interface{}, error) {
	l, err := convertValue(left, convertTo)
	if err != nil {
		return nil, nil, err
	}

	r, err := convertValue(right, convertTo)
	if err != nil {
		return nil, nil, err
	}

	return l, r, nil
}

// Type implements the Expression interface.
func (*comparison) Type() sql.Type {
	return sql.Boolean
}

// Left implements Comparer interface
func (c *comparison) Left() sql.Expression { return c.BinaryExpression.Left }

// Right implements Comparer interface
func (c *comparison) Right() sql.Expression { return c.BinaryExpression.Right }

// Equals is a comparison that checks an expression is equal to another.
type Equals struct {
	comparison
}

// NewEquals returns a new Equals expression.
func NewEquals(left sql.Expression, right sql.Expression) *Equals {
	return &Equals{newComparison(left, right)}
}

// Eval implements the Expression interface.
func (e *Equals) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := e.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == 0, nil
}

// WithChildren implements the Expression interface.
func (e *Equals) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 2)
	}
	return NewEquals(children[0], children[1]), nil
}

func (e *Equals) String() string {
	return fmt.Sprintf("%s = %s", e.Left(), e.Right())
}

func (e *Equals) DebugString() string {
	return fmt.Sprintf("%s = %s", sql.DebugString(e.Left()), sql.DebugString(e.Right()))
}

// Regexp is a comparison that checks an expression matches a regexp.
type Regexp struct {
	comparison
	pool   *sync.Pool
	cached bool
}

// NewRegexp creates a new Regexp expression.
func NewRegexp(left sql.Expression, right sql.Expression) *Regexp {
	var cached = true
	sql.Inspect(right, func(e sql.Expression) bool {
		if _, ok := e.(*GetField); ok {
			cached = false
		}
		return true
	})

	return &Regexp{
		comparison: newComparison(left, right),
		pool:       nil,
		cached:     cached,
	}
}

// Eval implements the Expression interface.
func (re *Regexp) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	if sql.IsText(re.Left().Type()) && sql.IsText(re.Right().Type()) {
		return re.compareRegexp(ctx, row)
	}

	result, err := re.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == 0, nil
}

type matcherErrTuple struct {
	matcher regex.Matcher
	err     error
}

func (re *Regexp) compareRegexp(ctx *sql.Context, row sql.Row) (interface{}, error) {
	left, err := re.Left().Eval(ctx, row)
	if err != nil || left == nil {
		return nil, err
	}
	left, err = sql.LongText.Convert(left)
	if err != nil {
		return nil, err
	}

	var (
		matcher  regex.Matcher
		disposer regex.Disposer
		right    interface{}
	)
	// eval right and convert to text
	if !re.cached || re.pool == nil {
		right, err = re.Right().Eval(ctx, row)
		if err != nil || right == nil {
			return nil, err
		}
		right, err = sql.LongText.Convert(right)
		if err != nil {
			return nil, err
		}
	}
	// for non-cached regex every time create a new matcher
	if !re.cached {
		matcher, disposer, err = regex.New(regex.Default(), right.(string))
	} else {
		if re.pool == nil {
			re.pool = &sync.Pool{
				New: func() interface{} {
					r, _, e := regex.New(regex.Default(), right.(string))
					return matcherErrTuple{r, e}
				},
			}
		}

		if obj := re.pool.Get(); obj != nil {
			met := obj.(matcherErrTuple)
			matcher = met.matcher
			err = met.err
		}
	}

	if matcher == nil {
		return nil, ErrInvalidRegexp.New(err.Error())
	}

	ok := matcher.Match(left.(string))

	if !re.cached {
		disposer.Dispose()
	} else if re.pool != nil {
		re.pool.Put(matcher)
	}
	return ok, nil
}

// WithChildren implements the Expression interface.
func (re *Regexp) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(re, len(children), 2)
	}
	return NewRegexp(children[0], children[1]), nil
}

func (re *Regexp) String() string {
	return fmt.Sprintf("%s REGEXP %s", re.Left(), re.Right())
}

func (re *Regexp) DebugString() string {
	return fmt.Sprintf("%s REGEXP %s", sql.DebugString(re.Left()), sql.DebugString(re.Right()))
}

// GreaterThan is a comparison that checks an expression is greater than another.
type GreaterThan struct {
	comparison
}

// NewGreaterThan creates a new GreaterThan expression.
func NewGreaterThan(left sql.Expression, right sql.Expression) *GreaterThan {
	return &GreaterThan{newComparison(left, right)}
}

// Eval implements the Expression interface.
func (gt *GreaterThan) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := gt.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == 1, nil
}

// WithChildren implements the Expression interface.
func (gt *GreaterThan) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(gt, len(children), 2)
	}
	return NewGreaterThan(children[0], children[1]), nil
}

func (gt *GreaterThan) String() string {
	return fmt.Sprintf("%s > %s", gt.Left(), gt.Right())
}

func (gt *GreaterThan) DebugString() string {
	return fmt.Sprintf("%s > %s", sql.DebugString(gt.Left()), sql.DebugString(gt.Right()))
}

// LessThan is a comparison that checks an expression is less than another.
type LessThan struct {
	comparison
}

// NewLessThan creates a new LessThan expression.
func NewLessThan(left sql.Expression, right sql.Expression) *LessThan {
	return &LessThan{newComparison(left, right)}
}

// Eval implements the expression interface.
func (lt *LessThan) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := lt.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result == -1, nil
}

// WithChildren implements the Expression interface.
func (lt *LessThan) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(lt, len(children), 2)
	}
	return NewLessThan(children[0], children[1]), nil
}

func (lt *LessThan) String() string {
	return fmt.Sprintf("%s < %s", lt.Left(), lt.Right())
}

func (lt *LessThan) DebugString() string {
	return fmt.Sprintf("%s < %s", sql.DebugString(lt.Left()), sql.DebugString(lt.Right()))
}

// GreaterThanOrEqual is a comparison that checks an expression is greater or equal to
// another.
type GreaterThanOrEqual struct {
	comparison
}

// NewGreaterThanOrEqual creates a new GreaterThanOrEqual
func NewGreaterThanOrEqual(left sql.Expression, right sql.Expression) *GreaterThanOrEqual {
	return &GreaterThanOrEqual{newComparison(left, right)}
}

// Eval implements the Expression interface.
func (gte *GreaterThanOrEqual) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := gte.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result > -1, nil
}

// WithChildren implements the Expression interface.
func (gte *GreaterThanOrEqual) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(gte, len(children), 2)
	}
	return NewGreaterThanOrEqual(children[0], children[1]), nil
}

func (gte *GreaterThanOrEqual) String() string {
	return fmt.Sprintf("%s >= %s", gte.Left(), gte.Right())
}

func (gte *GreaterThanOrEqual) DebugString() string {
	return fmt.Sprintf("%s >= %s", sql.DebugString(gte.Left()), sql.DebugString(gte.Right()))
}

// LessThanOrEqual is a comparison that checks an expression is equal or lower than
// another.
type LessThanOrEqual struct {
	comparison
}

// NewLessThanOrEqual creates a LessThanOrEqual expression.
func NewLessThanOrEqual(left sql.Expression, right sql.Expression) *LessThanOrEqual {
	return &LessThanOrEqual{newComparison(left, right)}
}

// Eval implements the Expression interface.
func (lte *LessThanOrEqual) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	result, err := lte.Compare(ctx, row)
	if err != nil {
		if ErrNilOperand.Is(err) {
			return nil, nil
		}

		return nil, err
	}

	return result < 1, nil
}

// WithChildren implements the Expression interface.
func (lte *LessThanOrEqual) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(lte, len(children), 2)
	}
	return NewLessThanOrEqual(children[0], children[1]), nil
}

func (lte *LessThanOrEqual) String() string {
	return fmt.Sprintf("%s <= %s", lte.Left(), lte.Right())
}

func (lte *LessThanOrEqual) DebugString() string {
	return fmt.Sprintf("%s <= %s", sql.DebugString(lte.Left()), sql.DebugString(lte.Right()))
}

var (
	// ErrUnsupportedInOperand is returned when there is an invalid righthand
	// operand in an IN operator.
	ErrUnsupportedInOperand = errors.NewKind("right operand in IN operation must be tuple, but is %T")
	// ErrInvalidOperandColumns is returned when the columns in the left operand
	// and the elements of the right operand don't match.
	ErrInvalidOperandColumns = errors.NewKind("operand should have %d columns, but has %d")
)
