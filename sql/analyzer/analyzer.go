package analyzer

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/sirupsen/logrus"
	"gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/go-mysql-server/sql"
)

const debugAnalyzerKey = "DEBUG_ANALYZER"

const maxAnalysisIterations = 1000

// ErrMaxAnalysisIters is thrown when the analysis iterations are exceeded
var ErrMaxAnalysisIters = errors.NewKind("exceeded max analysis iterations (%d)")

// ErrInAnalysis is thrown for generic analyzer errors
var ErrInAnalysis = errors.NewKind("error in analysis: %s")

// ErrInvalidNodeType is thrown when the analyzer can't handle a particular kind of node type
var ErrInvalidNodeType = errors.NewKind("%s: invalid node of type: %T")

// Builder provides an easy way to generate Analyzer with custom rules and options.
type Builder struct {
	preAnalyzeRules     []Rule
	postAnalyzeRules    []Rule
	preValidationRules  []Rule
	postValidationRules []Rule
	catalog             *sql.Catalog
	debug               bool
	parallelism         int
}

// NewBuilder creates a new Builder from a specific catalog.
// This builder allow us add custom Rules and modify some internal properties.
func NewBuilder(c *sql.Catalog) *Builder {
	return &Builder{catalog: c}
}

// WithDebug activates debug on the Analyzer.
func (ab *Builder) WithDebug() *Builder {
	ab.debug = true

	return ab
}

// WithParallelism sets the parallelism level on the analyzer.
func (ab *Builder) WithParallelism(parallelism int) *Builder {
	ab.parallelism = parallelism
	return ab
}

// AddPreAnalyzeRule adds a new rule to the analyze before the standard analyzer rules.
func (ab *Builder) AddPreAnalyzeRule(name string, fn RuleFunc) *Builder {
	ab.preAnalyzeRules = append(ab.preAnalyzeRules, Rule{name, fn})

	return ab
}

// AddPostAnalyzeRule adds a new rule to the analyzer after standard analyzer rules.
func (ab *Builder) AddPostAnalyzeRule(name string, fn RuleFunc) *Builder {
	ab.postAnalyzeRules = append(ab.postAnalyzeRules, Rule{name, fn})

	return ab
}

// AddPreValidationRule adds a new rule to the analyzer before standard validation rules.
func (ab *Builder) AddPreValidationRule(name string, fn RuleFunc) *Builder {
	ab.preValidationRules = append(ab.preValidationRules, Rule{name, fn})

	return ab
}

// AddPostValidationRule adds a new rule to the analyzer after standard validation rules.
func (ab *Builder) AddPostValidationRule(name string, fn RuleFunc) *Builder {
	ab.postValidationRules = append(ab.postValidationRules, Rule{name, fn})

	return ab
}

func init() {
	logrus.SetFormatter(simpleLogFormatter{})
}

// Build creates a new Analyzer using all previous data setted to the Builder
func (ab *Builder) Build() *Analyzer {
	_, debug := os.LookupEnv(debugAnalyzerKey)
	var batches = []*Batch{
		&Batch{
			Desc:       "pre-analyzer",
			Iterations: maxAnalysisIterations,
			Rules:      ab.preAnalyzeRules,
		},
		&Batch{
			Desc:       "once-before",
			Iterations: 1,
			Rules:      OnceBeforeDefault,
		},
		&Batch{
			Desc:       "default-rules",
			Iterations: maxAnalysisIterations,
			Rules:      DefaultRules,
		},
		&Batch{
			Desc:       "once-after",
			Iterations: 1,
			Rules:      OnceAfterDefault,
		},
		&Batch{
			Desc:       "post-analyzer",
			Iterations: maxAnalysisIterations,
			Rules:      ab.postAnalyzeRules,
		},
		&Batch{
			Desc:       "pre-validation",
			Iterations: 1,
			Rules:      ab.preValidationRules,
		},
		&Batch{
			Desc:       "validation",
			Iterations: 1,
			Rules:      DefaultValidationRules,
		},
		&Batch{
			Desc:       "post-validation",
			Iterations: 1,
			Rules:      ab.postValidationRules,
		},
		&Batch{
			Desc:       "after-all",
			Iterations: 1,
			Rules:      OnceAfterAll,
		},
	}

	return &Analyzer{
		Debug:        debug || ab.debug,
		contextStack: make([]string, 0),
		Batches:      batches,
		Catalog:      ab.catalog,
		Parallelism:  ab.parallelism,
	}
}

// Analyzer analyzes nodes of the execution plan and applies rules and validations
// to them.
type Analyzer struct {
	// Whether to log various debugging messages
	Debug bool
	// Whether to output the query plan at each step of the analyzer
	Verbose bool
	// A stack of debugger context. See PushDebugContext, PopDebugContext
	contextStack []string
	Parallelism  int
	// Batches of Rules to apply.
	Batches []*Batch
	// Catalog of databases and registered functions.
	Catalog *sql.Catalog
}

// NewDefault creates a default Analyzer instance with all default Rules and configuration.
// To add custom rules, the easiest way is use the Builder.
func NewDefault(c *sql.Catalog) *Analyzer {
	return NewBuilder(c).Build()
}

type simpleLogFormatter struct{}

func (s simpleLogFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	lvl := ""
	switch entry.Level {
	case logrus.PanicLevel:
		lvl = "PANIC"
	case logrus.FatalLevel:
		lvl = "FATAL"
	case logrus.ErrorLevel:
		lvl = "ERROR"
	case logrus.WarnLevel:
		lvl = "WARN"
	case logrus.InfoLevel:
		lvl = "INFO"
	case logrus.DebugLevel:
		lvl = "DEBUG"
	case logrus.TraceLevel:
		lvl = "TRACE"
	}

	msg := fmt.Sprintf("%s: %s\n", lvl, entry.Message)
	return ([]byte)(msg), nil
}

// Log prints an INFO message to stdout with the given message and args
// if the analyzer is in debug mode.
func (a *Analyzer) Log(msg string, args ...interface{}) {
	if a != nil && a.Debug {
		if len(a.contextStack) > 0 {
			ctx := strings.Join(a.contextStack, "/")
			logrus.Infof("%s: "+msg, append([]interface{}{ctx}, args...)...)
		} else {
			logrus.Infof(msg, args...)
		}
	}
}

// LogNode prints the node given if Verbose logging is enabled.
func (a *Analyzer) LogNode(n sql.Node) {
	if a != nil && n != nil && a.Verbose {
		if len(a.contextStack) > 0 {
			ctx := strings.Join(a.contextStack, "/")
			fmt.Printf("%s:\n%s", ctx, sql.DebugString(n))
		} else {
			fmt.Printf("%s", sql.DebugString(n))
		}
	}
}

// LogDiff logs the diff between the query plans after a transformation rules has been applied.
// Only can print a diff when the string representations of the nodes differ, which isn't always the case.
func (a *Analyzer) LogDiff(prev, next sql.Node) {
	if a.Debug && a.Verbose {
		if !reflect.DeepEqual(next, prev) {
			diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
				A:        difflib.SplitLines(sql.DebugString(prev)),
				B:        difflib.SplitLines(sql.DebugString(next)),
				FromFile: "Prev",
				FromDate: "",
				ToFile:   "Next",
				ToDate:   "",
				Context:  1,
			})
			if err != nil {
				panic(err)
			}
			if len(diff) > 0 {
				a.Log(diff)
			}
		}
	}
}

// PushDebugContext pushes the given context string onto the context stack, to use when logging debug messages.
func (a *Analyzer) PushDebugContext(msg string) {
	if a != nil {
		a.contextStack = append(a.contextStack, msg)
	}
}

// PopDebugContext pops a context message off the context stack.
func (a *Analyzer) PopDebugContext() {
	if a != nil && len(a.contextStack) > 0 {
		a.contextStack = a.contextStack[:len(a.contextStack)-1]
	}
}

// Analyze applies the transformation rules to the node given. In the case of an error, the last successfully
// transformed node is returned along with the error.
func (a *Analyzer) Analyze(ctx *sql.Context, n sql.Node, scope *Scope) (sql.Node, error) {
	span, ctx := ctx.Span("analyze", opentracing.Tags{
		"plan": n.String(),
	})

	var err error
	a.Log("starting analysis of node of type: %T", n)
	for _, batch := range a.Batches {
		a.PushDebugContext(batch.Desc)
		n, err = batch.Eval(ctx, a, n, scope)
		if err != nil {
			a.Log("Encountered error: %v", err)
			a.PopDebugContext()
			if ErrMaxAnalysisIters.Is(err) {
				continue
			}
			return n, err
		}
		a.PopDebugContext()
	}

	defer func() {
		if n != nil {
			span.SetTag("IsResolved", n.Resolved())
		}
		span.Finish()
	}()

	return n, err
}
