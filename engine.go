package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type (
	SelectorKey int16
	Value       interface{}
	Operator    func(ctx *Ctx, params []Value) (res Value, err error)
)

type Ctx struct {
	Selector
	Ctx context.Context
}

const (
	// node types
	nodeTypeMask = uint8(0b111)
	constant     = uint8(0b001)
	selector     = uint8(0b010)
	operator     = uint8(0b011)
	fastOperator = uint8(0b100)
	cond         = uint8(0b101)
	end          = uint8(0b110)
	debug        = uint8(0b111)

	// short circuit flag
	scIfFalse = uint8(0b001000)
	scIfTrue  = uint8(0b010000)
)

type node struct {
	flag     uint8
	childCnt int8
	scIdx    int16
	childIdx int16
	selKey   SelectorKey
	value    Value
	operator Operator
}

func (n *node) getNodeType() uint8 {
	return n.flag & nodeTypeMask
}

type Expr struct {
	maxStackSize int16
	nodes        []*node
	// extra info
	parentIdx []int16
	scIdx     []int16
	sfSize    []int16
	osSize    []int16
}

func Eval(expr string, vals map[string]interface{}, confs ...*CompileConfig) (Value, error) {
	var conf *CompileConfig
	if len(confs) > 1 {
		return nil, errors.New("error: too many compile configurations")
	}

	if len(confs) == 1 {
		conf = confs[0]
	} else {
		conf = NewCompileConfig(RegisterSelKeys(vals))
	}

	tree, err := Compile(conf, expr)
	if err != nil {
		return nil, err
	}

	return tree.Eval(NewCtxWithMap(conf, vals))
}

func (e *Expr) EvalBool(ctx *Ctx) (bool, error) {
	res, err := e.Eval(ctx)
	if err != nil {
		return false, err
	}
	v, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("invalid result type: %v", res)
	}
	return v, nil
}

func (e *Expr) Eval(ctx *Ctx) (Value, error) {
	var (
		size   = e.maxStackSize
		nodes  = e.nodes
		maxIdx = int16(-1)

		sf    []int16 // stack frame
		sfTop = int16(-1)

		os    []Value // operand stack
		osTop = int16(-1)

		scTriggered bool
	)

	// ensure that variables do not escape to the heap in most cases
	switch {
	case size <= 8:
		os = make([]Value, 8)
		sf = make([]int16, 8)
	case size <= 16:
		os = make([]Value, 16)
		sf = make([]int16, 16)
	default:
		os = make([]Value, size)
		sf = make([]int16, size)
	}

	var (
		curtIdx int16
		curt    *node

		res Value // result of current stack frame
		err error

		param  []Value
		param2 [2]Value
	)

	// push the root node to the stack frame
	// just increase the sfTop because the index of root node is zero,
	// so we don't need to actually push zero to stack
	// e.g. sf[sfTop+1], sfTop = 0, sfTop+1
	sfTop = 0

	for sfTop != -1 { // while stack frame is not empty
		curtIdx, sfTop = sf[sfTop], sfTop-1
		curt = nodes[curtIdx]

		switch curt.flag & nodeTypeMask {
		case fastOperator:
			cnt := int16(curt.childCnt)
			childIdx := curt.childIdx
			if cnt == 2 {
				param2[0], err = getNodeValue(ctx, nodes[childIdx])
				if err != nil {
					return nil, err
				}
				param2[1], err = getNodeValue(ctx, nodes[childIdx+1])
				if err != nil {
					return nil, err
				}
				param = param2[:]
			} else {
				param = make([]Value, cnt)
				for i := int16(0); i < cnt; i++ {
					child := nodes[childIdx+i]
					param[i], err = getNodeValue(ctx, child)
					if err != nil {
						return nil, err
					}
				}
			}

			res, err = curt.operator(ctx, param)
			if err != nil {
				return nil, fmt.Errorf("operator execution error, operator: %v, error: %w", curt.value, err)
			}
		case operator:
			cnt := int16(curt.childCnt)
			if curtIdx > maxIdx {
				// the node has never been visited before
				maxIdx = curtIdx
				sf[sfTop+1], sfTop = curtIdx, sfTop+1
				childIdx := curt.childIdx
				// push child nodes into the stack frame
				// the back nodes is on top
				if cnt == 2 {
					sf[sfTop+1], sfTop = childIdx+1, sfTop+1
					sf[sfTop+1], sfTop = childIdx, sfTop+1
				} else {
					sfTop = sfTop + cnt
					for i := int16(0); i < cnt; i++ {
						sf[sfTop-i] = childIdx + i
					}
				}
				continue
			}

			// current node has been visited
			maxIdx = curtIdx
			osTop = osTop - cnt
			if cnt == 2 {
				param2[0], param2[1] = os[osTop+1], os[osTop+2]
				param = param2[:]
			} else {
				param = make([]Value, cnt)
				copy(param, os[osTop+1:])
			}
			res, err = curt.operator(ctx, param)
			if err != nil {
				return nil, fmt.Errorf("operator execution error, operator: %v, error: %w", curt.value, err)
			}
		case selector:
			res, err = getSelectorValue(ctx, curt)
			if err != nil {
				return nil, err
			}
		case constant:
			res = curt.value
		case cond:
			childIdx := curt.childIdx
			if curtIdx > maxIdx {
				cnt := int16(curt.childCnt)

				maxIdx = curtIdx
				// push the end node to the stack frame
				sf[sfTop+1], sfTop = childIdx+cnt-1, sfTop+1
				sf[sfTop+1], sfTop = curtIdx, sfTop+1
				sf[sfTop+1], sfTop = childIdx, sfTop+1
			} else {
				res, osTop = os[osTop], osTop-1
				condRes, ok := res.(bool)
				if !ok {
					return nil, fmt.Errorf("eval error, result type of if condition should be bool, got: [%v]", res)
				}
				if condRes {
					sf[sfTop+1], sfTop = childIdx+1, sfTop+1
				} else {
					sf[sfTop+1], sfTop = childIdx+2, sfTop+1
				}
			}
			continue
		case end:
			maxIdx = e.parentIdx[curtIdx]
			res, osTop = os[osTop], osTop-1
		default:
			// only debug node will enter this branch
			offset := int16(len(nodes)) / 2
			debugStackFrame(sf, sfTop, offset)

			// push the real node to print stacks
			sf[sfTop+1], sfTop = curtIdx+offset, sfTop+1

			e.printStacks(scTriggered, maxIdx, os, osTop, sf, sfTop)
			scTriggered = false
			continue
		}

		// short circuit
		if b, ok := res.(bool); ok {
			for (!b && curt.flag&scIfFalse == scIfFalse) ||
				(b && curt.flag&scIfTrue == scIfTrue) {

				curtIdx = curt.scIdx
				if curtIdx == 0 {
					return res, nil
				}

				scTriggered = true

				maxIdx = curtIdx
				sfTop = e.sfSize[curtIdx] - 2
				osTop = e.osSize[curtIdx] - 1
				curt = nodes[curtIdx]
			}
		}

		// push the result of current frame to operator stack
		os[osTop+1], osTop = res, osTop+1
	}
	return os[0], nil
}

func unifyType(val Value) Value {
	switch v := val.(type) {
	case int:
		return int64(v)
	case time.Time:
		return v.Unix()
	case time.Duration:
		return int64(v / time.Second)
	case []int:
		temp := make([]int64, len(v))
		for i, iv := range v {
			temp[i] = int64(iv)
		}
		return temp
	case int32:
		return int64(v)
	case int16:
		return int64(v)
	case int8:
		return int64(v)
	case uint64:
		return int64(v)
	case uint32:
		return int64(v)
	case uint16:
		return int64(v)
	case uint8:
		return int64(v)
	}
	return val
}

func getNodeValue(ctx *Ctx, n *node) (res Value, err error) {
	if n.flag&nodeTypeMask == constant {
		res = n.value
	} else {
		res, err = getSelectorValue(ctx, n)
	}
	return
}

func getSelectorValue(ctx *Ctx, n *node) (res Value, err error) {
	res, err = ctx.Get(n.selKey, n.value.(string))
	if err != nil {
		return
	}

	switch res.(type) {
	case bool, string, int64, []int64, []string:
		return
	default:
		return unifyType(res), nil
	}
}

func debugStackFrame(sf []int16, sfTop, offset int16) {
	// replace with debug node
	for i := int16(0); i < sfTop; i++ {
		if sf[i] >= offset {
			sf[i] -= offset
		}
	}
}

func (e *Expr) printStacks(scTriggered bool, maxIdx int16, os []Value, osTop int16, sf []int16, sfTop int16) {
	if scTriggered {
		fmt.Printf("short circuit triggered\n\n")
	}
	var sb strings.Builder

	offset := int16(len(e.nodes)) / 2

	fmt.Printf("maxIdx:%d, sfTop:%d, osTop:%d\n", maxIdx-offset, sfTop, osTop)
	sb.WriteString(fmt.Sprintf("%15s", "Stack Frame: "))
	for i := sfTop; i >= 0; i-- {
		sb.WriteString(fmt.Sprintf("|%4v", e.nodes[sf[i]].value))
	}
	sb.WriteString("|\n")

	sb.WriteString(fmt.Sprintf("%15s", "Operand Stack: "))
	for i := osTop; i >= 0; i-- {
		sb.WriteString(fmt.Sprintf("|%4v", os[i]))
	}
	sb.WriteString("|\n")
	fmt.Println(sb.String())
}
