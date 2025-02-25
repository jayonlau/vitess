/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/semantics"

	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
)

var _ logicalPlan = (*routeGen4)(nil)

// routeGen4 is used to build a Route primitive.
// It's used to build one of the Select routes like
// SelectScatter, etc. Portions of the original Select AST
// are moved into this node, which will be used to build
// the final SQL for this route.
type routeGen4 struct {
	gen4Plan

	// Select is the AST for the query fragment that will be
	// executed by this route.
	Select sqlparser.SelectStatement

	// condition stores the AST condition that will be used
	// to resolve the ERoute Values field.
	condition sqlparser.Expr

	// eroute is the primitive being built.
	eroute *engine.Route

	// tables keeps track of which tables this route is covering
	tables semantics.TableSet
}

// Primitive implements the logicalPlan interface
func (rb *routeGen4) Primitive() engine.Primitive {
	return rb.eroute
}

// SetLimit adds a LIMIT clause to the route.
func (rb *routeGen4) SetLimit(limit *sqlparser.Limit) {
	rb.Select.SetLimit(limit)
}

// WireupGen4 implements the logicalPlan interface
func (rb *routeGen4) WireupGen4(_ *semantics.SemTable) error {
	rb.prepareTheAST()

	rb.eroute.Query = sqlparser.String(rb.Select)

	buffer := sqlparser.NewTrackedBuffer(sqlparser.FormatImpossibleQuery)
	node := buffer.WriteNode(rb.Select)
	query := node.ParsedQuery()
	rb.eroute.FieldQuery = query.Query
	return nil
}

// ContainsTables implements the logicalPlan interface
func (rb *routeGen4) ContainsTables() semantics.TableSet {
	return rb.tables
}

// prepareTheAST does minor fixups of the SELECT struct before producing the query string
func (rb *routeGen4) prepareTheAST() {
	_ = sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch node := node.(type) {
		case *sqlparser.Select:
			if len(node.SelectExprs) == 0 {
				node.SelectExprs = []sqlparser.SelectExpr{
					&sqlparser.AliasedExpr{
						Expr: sqlparser.NewIntLiteral("1"),
					},
				}
			}
		case *sqlparser.ComparisonExpr:
			// 42 = colName -> colName = 42
			b := node.Operator == sqlparser.EqualOp
			value := sqlparser.IsValue(node.Left)
			name := sqlparser.IsColName(node.Right)
			if b &&
				value &&
				name {
				node.Left, node.Right = node.Right, node.Left
			}
		}
		return true, nil
	}, rb.Select)
}

// procureValues procures and converts the input into
// the expected types for rb.Values.
func (rb *routeGen4) procureValues(plan logicalPlan, jt *jointab, val sqlparser.Expr) (sqltypes.PlanValue, error) {
	switch val := val.(type) {
	case sqlparser.ValTuple:
		pv := sqltypes.PlanValue{}
		for _, val := range val {
			v, err := rb.procureValues(plan, jt, val)
			if err != nil {
				return pv, err
			}
			pv.Values = append(pv.Values, v)
		}
		return pv, nil
	case *sqlparser.ColName:
		joinVar := jt.Procure(plan, val, rb.Order())
		return sqltypes.PlanValue{Key: joinVar}, nil
	default:
		return sqlparser.NewPlanValue(val)
	}
}

func (rb *routeGen4) isLocal(col *sqlparser.ColName) bool {
	return col.Metadata.(*column).Origin() == rb
}

// generateFieldQuery generates a query with an impossible where.
// This will be used on the RHS node to fetch field info if the LHS
// returns no result.
func (rb *routeGen4) generateFieldQuery(sel sqlparser.SelectStatement, jt *jointab) string {
	formatter := func(buf *sqlparser.TrackedBuffer, node sqlparser.SQLNode) {
		switch node := node.(type) {
		case *sqlparser.ColName:
			if !rb.isLocal(node) {
				_, joinVar := jt.Lookup(node)
				buf.WriteArg(":", joinVar)
				return
			}
		case sqlparser.TableName:
			if !sqlparser.SystemSchema(node.Qualifier.String()) {
				node.Name.Format(buf)
				return
			}
			node.Format(buf)
			return
		}
		sqlparser.FormatImpossibleQuery(buf, node)
	}

	buffer := sqlparser.NewTrackedBuffer(formatter)
	node := buffer.WriteNode(sel)
	query := node.ParsedQuery()
	return query.Query
}

// Rewrite implements the logicalPlan interface
func (rb *routeGen4) Rewrite(inputs ...logicalPlan) error {
	if len(inputs) != 0 {
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "route: wrong number of inputs")
	}
	return nil
}

// Inputs implements the logicalPlan interface
func (rb *routeGen4) Inputs() []logicalPlan {
	return []logicalPlan{}
}

func (rb *routeGen4) isSingleShard() bool {
	switch rb.eroute.Opcode {
	case engine.SelectUnsharded, engine.SelectDBA, engine.SelectNext, engine.SelectEqualUnique, engine.SelectReference:
		return true
	}
	return false
}

func (rb *routeGen4) unionCanMerge(other *routeGen4, distinct bool) bool {
	if rb.eroute.Keyspace.Name != other.eroute.Keyspace.Name {
		return false
	}
	switch rb.eroute.Opcode {
	case engine.SelectUnsharded, engine.SelectReference:
		return rb.eroute.Opcode == other.eroute.Opcode
	case engine.SelectDBA:
		return other.eroute.Opcode == engine.SelectDBA &&
			len(rb.eroute.SysTableTableSchema) == 0 &&
			len(rb.eroute.SysTableTableName) == 0 &&
			len(other.eroute.SysTableTableSchema) == 0 &&
			len(other.eroute.SysTableTableName) == 0
	case engine.SelectEqualUnique:
		// Check if they target the same shard.
		if other.eroute.Opcode == engine.SelectEqualUnique && rb.eroute.Vindex == other.eroute.Vindex && valEqual(rb.condition, other.condition) {
			return true
		}
	case engine.SelectScatter:
		return other.eroute.Opcode == engine.SelectScatter && !distinct
	case engine.SelectNext:
		return false
	}
	return false
}

func (rb *routeGen4) updateRoute(opcode engine.RouteOpcode, vindex vindexes.SingleColumn, condition sqlparser.Expr) {
	rb.eroute.Opcode = opcode
	rb.eroute.Vindex = vindex
	rb.condition = condition
}

// computeNotInPlan looks for null values to produce a SelectNone if found
func (rb *routeGen4) computeNotInPlan(right sqlparser.Expr) engine.RouteOpcode {
	switch node := right.(type) {
	case sqlparser.ValTuple:
		for _, n := range node {
			if sqlparser.IsNull(n) {
				return engine.SelectNone
			}
		}
	}

	return engine.SelectScatter
}

// exprIsValue returns true if the expression can be treated as a value
// for the routeOption. External references are treated as value.
func (rb *routeGen4) exprIsValue(expr sqlparser.Expr) bool {
	if node, ok := expr.(*sqlparser.ColName); ok {
		return node.Metadata.(*column).Origin() != rb
	}
	return sqlparser.IsValue(expr)
}
