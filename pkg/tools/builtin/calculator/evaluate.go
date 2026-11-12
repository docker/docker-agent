package calculator

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const (
	maxExpressionBytes = 8192
	maxDepth           = 64
	maxBits            = 4096
	maxExponent        = 1024
)

type parser struct {
	input string
	pos   int
	depth int
}

func evaluate(ctx context.Context, expression string) (*big.Rat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(expression) > maxExpressionBytes {
		return nil, fmt.Errorf("expression exceeds %d bytes", maxExpressionBytes)
	}
	p := parser{input: expression}
	value, err := p.sum(ctx)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.input) {
		return nil, fmt.Errorf("unexpected character at position %d; use only numbers, parentheses, +, -, *, /, %% and ^", p.pos+1)
	}
	return value, nil
}

func (p *parser) skipSpace() {
	for p.pos < len(p.input) && strings.ContainsRune(" \t\r\n", rune(p.input[p.pos])) {
		p.pos++
	}
}

func (p *parser) take(chars string) byte {
	p.skipSpace()
	if p.pos < len(p.input) && strings.ContainsRune(chars, rune(p.input[p.pos])) {
		ch := p.input[p.pos]
		p.pos++
		return ch
	}
	return 0
}

func (p *parser) sum(ctx context.Context) (*big.Rat, error) {
	left, err := p.product(ctx)
	if err != nil {
		return nil, err
	}
	for {
		op := p.take("+-")
		if op == 0 {
			return left, nil
		}
		right, err := p.product(ctx)
		if err != nil {
			return nil, err
		}
		if op == '+' {
			left.Add(left, right)
		} else {
			left.Sub(left, right)
		}
		if err := checkSize(left); err != nil {
			return nil, err
		}
	}
}

func (p *parser) product(ctx context.Context) (*big.Rat, error) {
	left, err := p.unary(ctx)
	if err != nil {
		return nil, err
	}
	for {
		op := p.take("*/%")
		if op == 0 {
			return left, nil
		}
		right, err := p.unary(ctx)
		if err != nil {
			return nil, err
		}
		switch op {
		case '*':
			left.Mul(left, right)
		case '/':
			if right.Sign() == 0 {
				return nil, errors.New("division by zero")
			}
			left.Quo(left, right)
		case '%':
			if !left.IsInt() || !right.IsInt() {
				return nil, errors.New("remainder requires integer operands; for percentages use / 100")
			}
			if right.Sign() == 0 {
				return nil, errors.New("remainder by zero")
			}
			left.SetInt(new(big.Int).Rem(left.Num(), right.Num()))
		}
		if err := checkSize(left); err != nil {
			return nil, err
		}
	}
}

func (p *parser) unary(ctx context.Context) (*big.Rat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxDepth {
		return nil, fmt.Errorf("expression nesting exceeds %d levels", maxDepth)
	}
	if op := p.take("+-"); op != 0 {
		value, err := p.unary(ctx)
		if err != nil {
			return nil, err
		}
		if op == '-' {
			value.Neg(value)
		}
		return value, nil
	}

	left, err := p.primary(ctx)
	if err != nil {
		return nil, err
	}
	if p.take("^") == 0 {
		return left, nil
	}
	// Parsing the exponent as unary makes powers right-associative and accepts 2^-3.
	right, err := p.unary(ctx)
	if err != nil {
		return nil, err
	}
	return power(left, right)
}

func (p *parser) primary(ctx context.Context) (*big.Rat, error) {
	if p.take("(") != 0 {
		value, err := p.sum(ctx)
		if err != nil {
			return nil, err
		}
		if p.take(")") == 0 {
			return nil, fmt.Errorf("expected ')' at position %d", p.pos+1)
		}
		return value, nil
	}
	return p.number()
}

func (p *parser) number() (*big.Rat, error) {
	p.skipSpace()
	start := p.pos
	p.digits()
	if p.pos < len(p.input) && p.input[p.pos] == '.' {
		p.pos++
		p.digits()
	}
	if p.pos == start || p.input[start:p.pos] == "." {
		return nil, fmt.Errorf("expected a number at position %d", start+1)
	}
	if p.pos < len(p.input) && (p.input[p.pos] == 'e' || p.input[p.pos] == 'E') {
		p.pos++
		exponentStart := p.pos
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			p.pos++
		}
		p.digits()
		exponent, err := strconv.Atoi(p.input[exponentStart:p.pos])
		if err != nil || exponent < -maxExponent || exponent > maxExponent {
			return nil, fmt.Errorf("scientific exponent must be an integer between -%d and %d", maxExponent, maxExponent)
		}
	}

	value, ok := new(big.Rat).SetString(p.input[start:p.pos])
	if !ok {
		return nil, fmt.Errorf("invalid number at position %d", start+1)
	}
	return value, checkSize(value)
}

func (p *parser) digits() {
	for p.pos < len(p.input) && p.input[p.pos] >= '0' && p.input[p.pos] <= '9' {
		p.pos++
	}
}

func power(base, exponent *big.Rat) (*big.Rat, error) {
	if !exponent.IsInt() || !exponent.Num().IsInt64() {
		return nil, errors.New("power exponent must be an integer between -1024 and 1024")
	}
	n := exponent.Num().Int64()
	if n < -maxExponent || n > maxExponent {
		return nil, errors.New("power exponent must be an integer between -1024 and 1024")
	}
	if base.Sign() == 0 && n <= 0 {
		return nil, errors.New("zero cannot be raised to a zero or negative power")
	}
	if n < 0 {
		base.Inv(base)
		n = -n
	}
	// Bound allocation before exponentiation, not just the resulting value.
	if int64(base.Num().BitLen())*n > maxBits || int64(base.Denom().BitLen())*n > maxBits {
		return nil, errors.New("power exceeds calculator size limit")
	}
	e := big.NewInt(n)
	numerator := new(big.Int).Exp(base.Num(), e, nil)
	denominator := new(big.Int).Exp(base.Denom(), e, nil)
	value := new(big.Rat).SetFrac(numerator, denominator)
	return value, checkSize(value)
}

func checkSize(value *big.Rat) error {
	if value.Num().BitLen() > maxBits || value.Denom().BitLen() > maxBits {
		return errors.New("value exceeds calculator size limit")
	}
	return nil
}
