//! Exact arithmetic; grammar and limits mirror pkg/tools/builtin/calculator.

use dashu_base::{BitTest, DivRem, Inverse, Sign, UnsignedAbs};
use dashu_int::UBig;
use dashu_ratio::RBig;
use serde::de::IgnoredAny;
use serde::{Deserialize, Serialize};

const MAX_BYTES: usize = 8192;
const MAX_ARGUMENT_BYTES: usize = 64 * 1024;
const MAX_DEPTH: u8 = 64;
const MAX_BITS: usize = 4096;
const MAX_EXPONENT: i32 = 1024;
const TOO_LONG: &str = "expression exceeds 8192 bytes";
const SCIENTIFIC_EXPONENT: &str = "scientific exponent must be an integer between -1024 and 1024";

pub fn calculate(arguments: &str) -> Result<String, String> {
    // Even a fully Unicode-escaped expression fits; reject unbounded arguments before parsing.
    if arguments.len() > MAX_ARGUMENT_BYTES {
        return Err(TOO_LONG.into());
    }
    let expression = expression_argument(arguments)?;
    if expression.len() > MAX_BYTES {
        return Err(TOO_LONG.into());
    }
    let mut parser = Parser {
        input: &expression,
        pos: 0,
    };
    let value = parser.sum(1)?;
    parser.space();
    if parser.pos != expression.len() {
        return Err(format!("unexpected character at position {}; use only numbers, parentheses, +, -, *, /, % and ^", parser.pos + 1));
    }
    Ok(answer(&value))
}

fn json_error(error: serde_json::Error) -> String {
    if error.is_eof() {
        "unexpected end of JSON input".into()
    } else {
        error.to_string()
    }
}

fn json_type(value: &str) -> &'static str {
    match value.as_bytes()[0] {
        b'n' => "null",
        b'{' => "object",
        b'[' => "array",
        b'"' => "string",
        b't' | b'f' => "bool",
        _ => "number",
    }
}

fn take_json<'de, T: Deserialize<'de>>(input: &mut &'de str) -> Result<T, String> {
    let mut stream = serde_json::Deserializer::from_str(input).into_iter();
    let value = stream
        .next()
        .ok_or("unexpected end of JSON input")?
        .map_err(json_error)?;
    *input = input[stream.byte_offset()..].trim_start();
    Ok(value)
}

fn expression_argument(arguments: &str) -> Result<String, String> {
    if arguments.is_empty() {
        return Ok(String::new());
    }
    // Validate the whole envelope before type errors, without materializing ignored values.
    serde_json::from_str::<IgnoredAny>(arguments).map_err(json_error)?;
    let json = arguments.trim_start();
    if json.trim_end() == "null" {
        return Ok(String::new());
    }
    let Some(mut fields) = json.strip_prefix('{') else {
        return Err(format!(
            "json: cannot unmarshal {} into Go value of type calculator.Args",
            json_type(json)
        ));
    };
    let mut expression = String::new();
    // Decode in wire order: unknown fields and null strings leave the current value intact.
    while !fields.trim_start().starts_with('}') {
        let key: String = take_json(&mut fields)?;
        fields = fields
            .strip_prefix(':')
            .expect("validated object field")
            .trim_start();
        match (key.eq_ignore_ascii_case("expression"), json_type(fields)) {
            (false, _) | (_, "null") => {
                take_json::<IgnoredAny>(&mut fields)?;
            }
            (true, "string") => expression = take_json(&mut fields)?,
            (_, ty) => {
                return Err(format!(
                    "json: cannot unmarshal {ty} into Go struct field Args.{key} of type string"
                ))
            }
        }
        let Some(rest) = fields.strip_prefix(',') else {
            break;
        };
        fields = rest;
    }
    Ok(expression)
}

struct Parser<'a> {
    input: &'a str,
    pos: usize,
}

type EvalResult = Result<RBig, String>;

impl<'a> Parser<'a> {
    fn peek(&self) -> Option<u8> {
        self.input.as_bytes().get(self.pos).copied()
    }

    fn space(&mut self) {
        while matches!(self.peek(), Some(b' ' | b'\t' | b'\n' | b'\r')) {
            self.pos += 1;
        }
    }

    fn take(&mut self, chars: &[u8]) -> Option<u8> {
        self.space();
        let ch = self.peek().filter(|ch| chars.contains(ch))?;
        self.pos += 1;
        Some(ch)
    }

    fn sum(&mut self, depth: u8) -> EvalResult {
        let mut left = self.product(depth)?;
        while let Some(op) = self.take(b"+-") {
            let right = self.product(depth)?;
            left = bounded(if op == b'+' {
                left + right
            } else {
                left - right
            })?;
        }
        Ok(left)
    }

    fn product(&mut self, depth: u8) -> EvalResult {
        let mut left = self.unary(depth)?;
        while let Some(op) = self.take(b"*/%") {
            let right = self.unary(depth)?;
            left = bounded(match op {
                b'*' => left * right,
                b'/' if right.is_zero() => return Err("division by zero".into()),
                b'/' => left / right,
                _ if !left.is_int() || !right.is_int() => {
                    return Err(
                        "remainder requires integer operands; for percentages use / 100".into(),
                    )
                }
                _ if right.is_zero() => return Err("remainder by zero".into()),
                _ => RBig::from(left.numerator() % right.numerator()),
            })?;
        }
        Ok(left)
    }

    fn unary(&mut self, depth: u8) -> EvalResult {
        if depth > MAX_DEPTH {
            return Err("expression nesting exceeds 64 levels".into());
        }
        if let Some(sign) = self.take(b"+-") {
            let value = self.unary(depth + 1)?;
            return Ok(if sign == b'-' { -value } else { value });
        }
        let base = if self.take(b"(").is_some() {
            let value = self.sum(depth + 1)?;
            if self.take(b")").is_none() {
                return Err(format!("expected ')' at position {}", self.pos + 1));
            }
            value
        } else {
            self.number()?
        };
        // An exponent is unary, not primary: 2^-3^2 is 2^(-(3^2)).
        if self.take(b"^").is_some() {
            power(base, self.unary(depth + 1)?)
        } else {
            Ok(base)
        }
    }

    fn digits(&mut self) -> &'a str {
        let start = self.pos;
        while self.peek().is_some_and(|ch| ch.is_ascii_digit()) {
            self.pos += 1;
        }
        &self.input[start..self.pos]
    }

    fn number(&mut self) -> EvalResult {
        let start = self.pos;
        let whole = self.digits();
        let fraction = if self.peek() == Some(b'.') {
            self.pos += 1;
            self.digits()
        } else {
            ""
        };
        if whole.is_empty() && fraction.is_empty() {
            return Err(format!("expected a number at position {}", start + 1));
        }
        let mut exponent = 0;
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.pos += 1;
            let start = self.pos;
            if matches!(self.peek(), Some(b'+' | b'-')) {
                self.pos += 1;
            }
            self.digits();
            exponent = self.input[start..self.pos]
                .parse::<i32>()
                .ok()
                .filter(|n| (-MAX_EXPONENT..=MAX_EXPONENT).contains(n))
                .ok_or(SCIENTIFIC_EXPONENT)?;
        }
        let digits = [whole, fraction].concat();
        let coefficient = digits.parse::<UBig>().expect("decimal digits");
        if coefficient.is_zero() {
            return Ok(RBig::ZERO);
        }
        // Before reduction, scale is bounded by the input length plus MAX_EXPONENT.
        let scale = exponent - fraction.len() as i32;
        let factor = UBig::from(10u8).pow(scale.unsigned_abs() as usize);
        bounded(if scale < 0 {
            RBig::from_parts(coefficient.into(), factor)
        } else {
            RBig::from(coefficient * factor)
        })
    }
}

fn bit_len(value: &RBig) -> usize {
    value
        .numerator()
        .bit_len()
        .max(value.denominator().bit_len())
}

fn bounded(value: RBig) -> EvalResult {
    if bit_len(&value) > MAX_BITS {
        Err("value exceeds calculator size limit".into())
    } else {
        Ok(value)
    }
}

fn power(base: RBig, exponent: RBig) -> EvalResult {
    let n = i32::try_from(exponent.numerator())
        .ok()
        .filter(|n| exponent.is_int() && (-MAX_EXPONENT..=MAX_EXPONENT).contains(n))
        .ok_or("power exponent must be an integer between -1024 and 1024")?;
    if base.is_zero() && n <= 0 {
        return Err("zero cannot be raised to a zero or negative power".into());
    }
    // Match the conservative Go preflight even when the actual result would fit.
    let abs_n = n.unsigned_abs() as usize;
    if bit_len(&base) * abs_n > MAX_BITS {
        return Err("power exceeds calculator size limit".into());
    }
    let result = base.pow(abs_n);
    Ok(if n < 0 { result.inv() } else { result })
}

fn decimal_places(denominator: &UBig) -> Option<usize> {
    // A reduced fraction terminates iff the denominator has only factors of 2 and 5.
    let twos = denominator.trailing_zeros().expect("positive denominator");
    let mut rest = denominator >> twos;
    let mut fives = 0;
    let five = UBig::from(5u8);
    while !rest.is_one() {
        let (q, r) = rest.div_rem(&five);
        if !r.is_zero() {
            return None;
        }
        rest = q;
        fives += 1;
    }
    Some(twos.max(fives))
}

fn answer(value: &RBig) -> String {
    #[derive(Serialize)]
    struct Answer<'a> {
        result: &'a str,
        decimal: &'a str,
        approximate: bool,
    }

    let denominator = value.denominator();
    let places = decimal_places(denominator);
    let precision = places.unwrap_or(20);
    let scaled = value.numerator().unsigned_abs() * UBig::from(10u8).pow(precision);
    let (mut rounded, remainder) = scaled.div_rem(denominator);
    if remainder << 1usize >= *denominator {
        rounded += 1u8;
    }
    let mut decimal = format!("{rounded:0width$}", width = precision + 1);
    if precision != 0 {
        decimal.insert(decimal.len() - precision, '.');
    }
    if value.sign() == Sign::Negative {
        decimal.insert(0, '-');
    }
    let fraction = places.is_none().then(|| value.to_string());
    serde_json::to_string(&Answer {
        result: fraction.as_deref().unwrap_or(&decimal),
        decimal: &decimal,
        approximate: places.is_none(),
    })
    .expect("serializing strings and bool")
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn run(expression: &str) -> Result<String, String> {
        calculate(&json!({"expression": expression}).to_string())
    }

    #[test]
    fn exact_arithmetic() {
        for (expr, want) in [
            ("2 + 3 * 4", "14"),
            ("(2 + 3) * 4", "20"),
            ("\t-12 + +2\r\n", "-10"),
            ("0.1 + 0.2", "0.3"),
            ("(19.99 * 3) * (1 + 8.25 / 100)", "64.917525"),
            ("9007199254740993 + 1", "9007199254740994"),
            ("9223372036854775807 * 2", "18446744073709551614"),
            ("1 / 8", "0.125"),
            ("1 / 3 * 3", "1"),
            ("1/3 + 1/6", "0.5"),
            ("1e3 + 2.5E-2", "1000.025"),
            ("1e+2", "100"),
            (".5 + 1.", "1.5"),
            ("010 + 008", "18"),
            ("00009", "9"),
            ("010 / 008", "1.25"),
            ("010e0 + 008.0", "18"),
            ("-000.0", "0"),
            (".0", "0"),
            ("10 / 4 / 2", "1.25"),
            ("10 - 4 - 2", "4"),
            ("17 % 5", "2"),
            ("-17 % 5", "-2"),
            ("17 % -5", "2"),
            ("2^10", "1024"),
            ("2^-3", "0.125"),
            ("(1/3)^-2", "9"),
            ("2^3^2", "512"),
            ("-2^2", "-4"),
            ("(-2)^2", "4"),
            ("2^0", "1"),
            ("0^2", "0"),
            ("(-2)^3", "-8"),
            ("2^-3^2", "0.001953125"),
        ] {
            assert_eq!(
                run(expr).unwrap(),
                format!(r#"{{"result":"{want}","decimal":"{want}","approximate":false}}"#),
                "{expr}"
            );
        }
    }

    #[test]
    fn nonterminating_decimals() {
        for (expr, fraction, decimal) in [
            ("2/6", "1/3", "0.33333333333333333333"),
            ("2/3", "2/3", "0.66666666666666666667"),
            ("-1/6", "-1/6", "-0.16666666666666666667"),
            ("1/3 + 2^-3", "11/24", "0.45833333333333333333"),
            (
                "-1/3e20",
                "-1/300000000000000000000",
                "-0.00000000000000000000",
            ),
            (
                "(3e20 - 1) / 3e20",
                "299999999999999999999/300000000000000000000",
                "1.00000000000000000000",
            ),
        ] {
            assert_eq!(
                run(expr).unwrap(),
                format!(r#"{{"result":"{fraction}","decimal":"{decimal}","approximate":true}}"#),
                "{expr}"
            );
        }
    }

    #[test]
    fn errors_and_precedence() {
        for (expr, message) in [
            ("", "expected a number at position 1"),
            ("\t \r\n", "expected a number at position 5"),
            ("1 +", "expected a number at position 4"),
            ("(1 + 2  ", "expected ')' at position 9"),
            ("()", "expected a number at position 2"),
            (".", "expected a number at position 1"),
            ("sqrt(4)", "expected a number at position 1"),
            ("x", "expected a number at position 1"),
            ("1/ (2-2)", "division by zero"),
            ("1 / 0 + x", "division by zero"),
            ("1 % 0", "remainder by zero"),
            (
                "1.5 % 0",
                "remainder requires integer operands; for percentages use / 100",
            ),
            (
                "1 % .5",
                "remainder requires integer operands; for percentages use / 100",
            ),
            ("0^0", "zero cannot be raised to a zero or negative power"),
            ("0^-1", "zero cannot be raised to a zero or negative power"),
            ("(10^1000)^1000", "power exceeds calculator size limit"),
            ("1e1000*1e1000", "value exceeds calculator size limit"),
        ] {
            assert_eq!(run(expr).unwrap_err(), message, "{expr}");
        }
        for (expr, pos) in [
            ("1.2.3", 4),
            ("1,000", 2),
            ("2(3)", 2),
            ("1 + 2)", 6),
            ("1;alert(1)", 2),
            ("0xff", 2),
            ("1\u{a0}+2", 2),
        ] {
            assert_eq!(run(expr).unwrap_err(), format!("unexpected character at position {pos}; use only numbers, parentheses, +, -, *, /, % and ^"));
        }
        for expr in [
            "1e1025",
            "1e-1025",
            "1e99999999999999999999",
            "1e",
            "1e-",
            "3e+ 1",
        ] {
            assert_eq!(run(expr).unwrap_err(), SCIENTIFIC_EXPONENT, "{expr}");
        }
        for expr in ["4^0.5", "2^1025", "2^-1025", "2^99999999999999999999"] {
            assert_eq!(
                run(expr).unwrap_err(),
                "power exponent must be an integer between -1024 and 1024",
                "{expr}"
            );
        }
    }

    #[test]
    fn limit_boundaries() {
        for (open, close) in [("(", ")"), ("-", ""), ("1^", "")] {
            let nested = |n| format!("{}1{}", open.repeat(n), close.repeat(n));
            assert!(run(&nested(63)).is_ok());
            assert_eq!(
                run(&nested(64)).unwrap_err(),
                "expression nesting exceeds 64 levels"
            );
        }
        assert!(run(&"0".repeat(MAX_BYTES)).is_ok());
        assert!(calculate(&format!(
            r#"{{"expression":"{}"}}"#,
            r"\u0030".repeat(MAX_BYTES)
        ))
        .is_ok());
        assert_eq!(run(&"0".repeat(MAX_BYTES + 1)).unwrap_err(), TOO_LONG);
        assert_eq!(calculate(&"9".repeat(1 << 20)).unwrap_err(), TOO_LONG);
        assert_eq!(
            run(&"9".repeat(MAX_BYTES)).unwrap_err(),
            "value exceeds calculator size limit"
        );
        assert!(run(&format!("1e{}1024", "0".repeat(8000))).is_ok());
        assert!(run("1e-1024").is_ok());
        assert!(run("(2^1023)^4").is_ok());
        assert_eq!(
            run("(2^1024)^4").unwrap_err(),
            "power exceeds calculator size limit"
        );
        assert!(run("((2^1023)^4 * 8 + 1)^1").is_ok());
        assert_eq!(
            run("(2^1023)^4 * 16").unwrap_err(),
            "value exceeds calculator size limit"
        );
        assert_eq!(
            run("(1e1024)^-1024").unwrap_err(),
            "power exceeds calculator size limit"
        );
    }

    #[test]
    fn exact_results_can_be_reused() {
        for expr in ["(2^-1023)^4", "1/(2^1000-1)", "10^1000"] {
            let original = run(expr).unwrap();
            let fields: serde_json::Value = serde_json::from_str(&original).unwrap();
            let exact = fields["result"].as_str().unwrap();
            assert_eq!(run(&format!("({exact}) * 1")).unwrap(), original);
        }
    }

    #[test]
    fn argument_decoding() {
        for args in [
            r#"{ "ExPREssion":"1", "expression":"2", "unused":true }"#,
            r#"{"expression":"\u0032", "expression":null}"#,
            r#"{"unused":[1e999999999999], "expression":"2"}"#,
        ] {
            assert_eq!(calculate(args), run("2"));
        }
        for args in ["", "{}", "null", r#"{"expression":null}"#] {
            assert_eq!(calculate(args), run(""));
        }
        for (args, error) in [
            ("   ", "unexpected end of JSON input"),
            ("{", "unexpected end of JSON input"),
            (
                "[]",
                "json: cannot unmarshal array into Go value of type calculator.Args",
            ),
            (
                r#"{"ExpresSion":1e99999999}"#,
                "json: cannot unmarshal number into Go struct field Args.ExpresSion of type string",
            ),
            (
                r#"{"expression":[],"expression":"1"}"#,
                "json: cannot unmarshal array into Go struct field Args.expression of type string",
            ),
        ] {
            assert_eq!(calculate(args).unwrap_err(), error);
        }
        assert!(calculate(r#"{"expression":"2"}[]"#).is_err());
    }
}
