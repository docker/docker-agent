//! Go encoding/json conventions shared by typed builtin handlers.
use serde::{de::IgnoredAny, Serialize};
use std::{borrow::Cow, fmt::Write as _, io};

pub const MAX_JSON_DEPTH: usize = 10_000;

// serde_json already matches encoding/json for quotes, backslashes and control characters.
struct GoFormatter<const FLOAT_MAP: bool>;

impl<const FLOAT_MAP: bool> serde_json::ser::Formatter for GoFormatter<FLOAT_MAP> {
    fn write_u64<W: io::Write + ?Sized>(&mut self, w: &mut W, n: u64) -> io::Result<()> {
        if FLOAT_MAP {
            self.write_f64(w, n as f64)
        } else {
            serde_json::ser::CompactFormatter.write_u64(w, n)
        }
    }

    fn write_i64<W: io::Write + ?Sized>(&mut self, w: &mut W, n: i64) -> io::Result<()> {
        if FLOAT_MAP {
            self.write_f64(w, n as f64)
        } else {
            serde_json::ser::CompactFormatter.write_i64(w, n)
        }
    }

    fn write_f64<W: io::Write + ?Sized>(&mut self, w: &mut W, n: f64) -> io::Result<()> {
        if !FLOAT_MAP {
            return serde_json::ser::CompactFormatter.write_f64(w, n);
        }
        let text = serde_json::to_string(&n).unwrap();
        let Some((mantissa, exp)) = text.split_once('e') else {
            return w.write_all(text.trim_end_matches(".0").as_bytes());
        };
        let exp: i32 = exp.parse().unwrap();
        if !(-6..21).contains(&exp) {
            return write!(w, "{mantissa}e{exp:+}");
        }
        // Reposition the shortest digits when serde uses an exponent within Go's fixed range.
        let digits = mantissa.trim_start_matches('-').replace('.', "");
        if n < 0.0 {
            w.write_all(b"-")?;
        }
        if exp < 0 {
            write!(w, "0.{:0>width$}{digits}", "", width = (-exp - 1) as usize)
        } else {
            write!(w, "{digits:0<width$}", width = exp as usize + 1)
        }
    }

    fn write_string_fragment<W: io::Write + ?Sized>(
        &mut self,
        w: &mut W,
        s: &str,
    ) -> io::Result<()> {
        let mut start = 0;
        for (i, ch) in s.char_indices() {
            let escape = match ch {
                '<' => br"\u003c",
                '>' => br"\u003e",
                '&' => br"\u0026",
                '\u{2028}' => br"\u2028",
                '\u{2029}' => br"\u2029",
                _ => continue,
            };
            w.write_all(&s.as_bytes()[start..i])?;
            w.write_all(escape)?;
            start = i + ch.len_utf8();
        }
        w.write_all(&s.as_bytes()[start..])
    }
}

pub fn encode(value: impl Serialize) -> String {
    serialize(value, GoFormatter::<false>)
}

// Numbers decoded into Go's map[string]any are float64, even when written as integers.
pub fn encode_decoded(value: &serde_json::Value) -> String {
    serialize(value, GoFormatter::<true>)
}

fn serialize(value: impl Serialize, formatter: impl serde_json::ser::Formatter) -> String {
    let mut bytes = Vec::new();
    value
        .serialize(&mut serde_json::Serializer::with_formatter(
            &mut bytes, formatter,
        ))
        .unwrap();
    String::from_utf8(bytes).unwrap()
}

// Walk validated JSON in wire order. Go ignores unknown fields, merges duplicate
// struct keys and treats null as a no-op for string/struct destinations.
pub fn take<'a>(rest: &mut &'a str) -> &'a str {
    let s = rest.trim_start();
    let mut stream = serde_json::Deserializer::from_str(s).into_iter::<IgnoredAny>();
    stream.next().unwrap().unwrap();
    let end = stream.byte_offset();
    *rest = s[end..].trim_start();
    &s[..end]
}

pub fn fields(
    mut s: &str,
    mut field: impl FnMut(String, &str) -> Result<(), String>,
) -> Result<(), String> {
    s = s[1..].trim_start();
    let mut first_error = None;
    while !s.starts_with('}') {
        let key = serde_json::from_str(take(&mut s)).expect("validated JSON key");
        s = &s[1..]; // colon
        if let Err(e) = field(key, take(&mut s)) {
            first_error.get_or_insert(e);
        }
        let Some(next) = s.strip_prefix(',') else {
            break;
        };
        s = next;
    }
    first_error.map_or(Ok(()), Err)
}

pub fn field_matches(key: &str, field: &str) -> bool {
    key.eq_ignore_ascii_case(field)
        || (key.contains('ſ') && key.replace('ſ', "s").eq_ignore_ascii_case(field))
}

fn kind(s: &str) -> &str {
    match s.as_bytes()[0] {
        b'{' => "object",
        b'[' => "array",
        b'"' => "string",
        b't' | b'f' => "bool",
        b'n' => "null",
        _ => "number",
    }
}

pub fn type_error(val: &str, path: &str, ty: &str) -> String {
    format!(
        "json: cannot unmarshal {} into {path} of type {ty}",
        kind(val)
    )
}

// encoding/json converts its JSON pointer to a dotted field name, treating a final number as an index.
pub fn map_field_path(key: &str) -> String {
    if key.is_empty() {
        return "Go value".into();
    }
    let key = key.replace('~', "~0").replace('/', "~1");
    let tail = key.rsplit('.').next().unwrap();
    if !tail.is_empty() && tail.bytes().all(|b| b.is_ascii_digit()) {
        format!(".{key}")
    } else {
        format!("Go struct field .{key}")
    }
}

pub fn read_string(val: &str, s: &mut String, path: &str) -> Result<(), String> {
    if val.starts_with('"') {
        *s = serde_json::from_str(val).expect("validated JSON string");
    } else if val != "null" {
        return Err(type_error(val, path, "string"));
    }
    Ok(())
}

// encoding/json limits container depth and replaces each unpaired UTF-16 surrogate with U+FFFD.
pub fn normalize_json(s: &str) -> Result<Cow<'_, str>, String> {
    let mut bytes = Cow::Borrowed(s.as_bytes());
    let unit = |i: usize| -> Option<u16> {
        u16::from_str_radix(s.get(i..i + 6)?.strip_prefix(r"\u")?, 16).ok()
    };
    let mut i = 0;
    let mut quoted = false;
    let mut depth: usize = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'"' => quoted = !quoted,
            b'[' | b'{' if !quoted => {
                depth += 1;
                if depth > MAX_JSON_DEPTH {
                    return Err("exceeded max depth".into());
                }
            }
            b']' | b'}' if !quoted => depth = depth.saturating_sub(1),
            b'\\' if quoted => {
                match unit(i) {
                    Some(0xd800..=0xdbff) if matches!(unit(i + 6), Some(0xdc00..=0xdfff)) => {
                        i += 12
                    }
                    Some(0xd800..=0xdfff) => {
                        bytes.to_mut()[i + 2..i + 6].copy_from_slice(b"fffd");
                        i += 6;
                    }
                    _ => i += 2,
                }
                continue;
            }
            _ => (),
        }
        i += 1;
    }
    Ok(match bytes {
        Cow::Borrowed(_) => Cow::Borrowed(s),
        Cow::Owned(bytes) => Cow::Owned(String::from_utf8(bytes).unwrap()),
    })
}

/// The message needs Go's Unicode tables; callers fail closed.
#[derive(Debug)]
pub struct Unsupported;

enum Fault {
    End,
    Depth,
    Text(String),
    Unsupported,
}

/// What Go 1.27's encoding/json reports for invalid JSON text, `None` when `s` is valid.
/// Mirrors jsontext with the v1 options: duplicate names and lone surrogates are allowed.
pub fn syntax_error(s: &str) -> Result<Option<String>, Unsupported> {
    let mut scan = Scanner { s, pos: 0 };
    let checked = scan
        .space()
        .and_then(|()| scan.value())
        .and_then(|()| match scan.space() {
            Err(Fault::End) => Ok(()),
            Ok(()) => Err(scan.invalid("after top-level value")),
            Err(fault) => Err(fault),
        });
    match checked {
        Ok(()) => Ok(None),
        Err(Fault::End) => Ok(Some("unexpected end of JSON input".into())),
        Err(Fault::Depth) => Ok(Some("exceeded max depth".into())),
        Err(Fault::Text(message)) => Ok(Some(message)),
        Err(Fault::Unsupported) => Err(Unsupported),
    }
}

struct Scanner<'a> {
    s: &'a str,
    pos: usize,
}

impl Scanner<'_> {
    fn peek(&self) -> Option<u8> {
        self.s.as_bytes().get(self.pos).copied()
    }

    // Skips whitespace; the input must not end here.
    fn space(&mut self) -> Result<(), Fault> {
        while matches!(self.peek(), Some(b' ' | b'\t' | b'\r' | b'\n')) {
            self.pos += 1;
        }
        self.peek().map(|_| ()).ok_or(Fault::End)
    }

    fn invalid(&self, place: &str) -> Fault {
        let c = self.s[self.pos..].chars().next().expect("unread input");
        match quote_rune(c) {
            Some(quoted) => Fault::Text(format!("invalid character {quoted} {place}")),
            None => Fault::Unsupported,
        }
    }

    // Iterative: Go accepts 10,000 levels, more than the Wasm stack can recurse.
    fn value(&mut self) -> Result<(), Fault> {
        let mut open = Vec::new();
        loop {
            match self.peek().ok_or(Fault::End)? {
                b'n' => self.literal("null")?,
                b't' => self.literal("true")?,
                b'f' => self.literal("false")?,
                b'"' => self.string()?,
                b'-' | b'0'..=b'9' => self.number()?,
                bracket @ (b'{' | b'[') => {
                    if open.len() == MAX_JSON_DEPTH {
                        return Err(Fault::Depth);
                    }
                    self.pos += 1;
                    self.space()?;
                    if self.peek() != Some(bracket + 2) {
                        open.push(bracket);
                        self.key(bracket)?;
                        continue;
                    }
                    self.pos += 1;
                }
                _ => return Err(self.invalid("looking for beginning of value")),
            }
            // After a value: close finished containers or move to the next element.
            loop {
                let Some(&bracket) = open.last() else {
                    return Ok(());
                };
                self.space()?;
                match self.s.as_bytes()[self.pos] {
                    b',' => {
                        self.pos += 1;
                        self.space()?;
                        self.key(bracket)?;
                        break;
                    }
                    c if c == bracket + 2 => {
                        self.pos += 1;
                        open.pop();
                    }
                    _ => {
                        return Err(self.invalid(if bracket == b'{' {
                            "after object key:value pair"
                        } else {
                            "after array element"
                        }));
                    }
                }
            }
        }
    }

    // In an object, consumes the member name and colon before its value.
    fn key(&mut self, bracket: u8) -> Result<(), Fault> {
        if bracket == b'{' {
            if self.peek() != Some(b'"') {
                return Err(self.invalid("looking for beginning of object key string"));
            }
            self.string()?;
            self.space()?;
            if self.peek() != Some(b':') {
                return Err(self.invalid("after object key"));
            }
            self.pos += 1;
            self.space()?;
        }
        Ok(())
    }

    fn literal(&mut self, literal: &str) -> Result<(), Fault> {
        for expected in literal.bytes() {
            match self.peek() {
                None => return Err(Fault::End),
                Some(c) if c == expected => self.pos += 1,
                Some(_) => {
                    let place = format!(
                        "in literal {literal} (expecting '{}')",
                        char::from(expected)
                    );
                    return Err(self.invalid(&place));
                }
            }
        }
        Ok(())
    }

    fn string(&mut self) -> Result<(), Fault> {
        let b = self.s.as_bytes();
        self.pos += 1;
        loop {
            match *b.get(self.pos).ok_or(Fault::End)? {
                b'"' => {
                    self.pos += 1;
                    return Ok(());
                }
                b'\\' => {
                    let rest = &b[self.pos..];
                    let escape = *rest.get(1).ok_or(Fault::End)?;
                    let hex = |c: &u8| c.is_ascii_hexdigit();
                    self.pos += match escape {
                        b'"' | b'\\' | b'/' | b'b' | b'f' | b'n' | b'r' | b't' => 2,
                        b'u' if rest.len() < 6 => {
                            // A truncated but so far valid \uXXXX is an early end, not a bad escape.
                            return Err(if rest[2..].iter().all(hex) {
                                Fault::End
                            } else {
                                invalid_escape(rest)
                            });
                        }
                        b'u' if rest[2..6].iter().all(hex) => 6,
                        b'u' => return Err(invalid_escape(&rest[..6])),
                        _ => return Err(invalid_escape(&rest[..2])),
                    };
                }
                c if c < b' ' => return Err(self.invalid("in string")),
                _ => self.pos += 1,
            }
        }
    }

    fn number(&mut self) -> Result<(), Fault> {
        if self.peek() == Some(b'-') {
            self.pos += 1;
        }
        if self.peek() == Some(b'0') {
            self.pos += 1;
        } else {
            self.digits()?;
        }
        if self.peek() == Some(b'.') {
            self.pos += 1;
            self.digits()?;
        }
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.pos += 1;
            if matches!(self.peek(), Some(b'-' | b'+')) {
                self.pos += 1;
            }
            self.digits()?;
        }
        Ok(())
    }

    fn digits(&mut self) -> Result<(), Fault> {
        match self.peek() {
            None => return Err(Fault::End),
            Some(b'0'..=b'9') => (),
            Some(_) => return Err(self.invalid("in numeric literal")),
        }
        while matches!(self.peek(), Some(b'0'..=b'9')) {
            self.pos += 1;
        }
        Ok(())
    }
}

// strconv.QuoteRune for the runes whose printability needs no table.
fn quote_rune(c: char) -> Option<String> {
    Some(match c {
        '\'' => r"'\''".into(),
        '\\' => r"'\\'".into(),
        '\u{7}' => r"'\a'".into(),
        '\u{8}' => r"'\b'".into(),
        '\u{c}' => r"'\f'".into(),
        '\n' => r"'\n'".into(),
        '\r' => r"'\r'".into(),
        '\t' => r"'\t'".into(),
        '\u{b}' => r"'\v'".into(),
        ' '..='~' | '\u{a1}'..='\u{ac}' | '\u{ae}'..='\u{ff}' => format!("'{c}'"),
        '\0'..='\u{7f}' => format!(r"'\x{:02x}'", u32::from(c)),
        '\u{80}'..='\u{ff}' => format!(r"'\u{:04x}'", u32::from(c)),
        _ => return None,
    })
}

// InvalidTextError for an escape: backquoted when printable, else strconv.Quote.
fn invalid_escape(what: &[u8]) -> Fault {
    let mut quoted = String::new();
    let plain = what.iter().all(|c| c.is_ascii_graphic() && *c != b'`');
    for (i, &c) in what.iter().enumerate() {
        match c {
            _ if plain => quoted.push(char::from(c)),
            b'"' | b'\\' => quoted.extend(['\\', char::from(c)]),
            b' '..=b'~' => quoted.push(char::from(c)),
            7 => quoted.push_str(r"\a"),
            8 => quoted.push_str(r"\b"),
            12 => quoted.push_str(r"\f"),
            b'\n' => quoted.push_str(r"\n"),
            b'\r' => quoted.push_str(r"\r"),
            b'\t' => quoted.push_str(r"\t"),
            11 => quoted.push_str(r"\v"),
            // The two-byte slice of an escape can only split a character after its lead byte.
            _ if c.is_ascii() || (what.len() == 2 && i == 1) => {
                write!(quoted, r"\x{c:02x}").unwrap();
            }
            _ => return Fault::Unsupported,
        }
    }
    let quote = if plain { '`' } else { '"' };
    Fault::Text(format!(
        "invalid escape sequence {quote}{quoted}{quote} in string"
    ))
}

/// The object to decode into the Go type `ty` (e.g. `think.Args`), `None` for empty or null.
pub fn object<'a>(input: &'a str, ty: &str) -> Result<Option<&'a str>, String> {
    if input.is_empty() {
        return Ok(None);
    }
    let input = input.trim_matches([' ', '\t', '\r', '\n']);
    if let Some(error) = syntax_error(input).map_err(|_| "invalid JSON arguments")? {
        return Err(error);
    }
    if input == "null" {
        return Ok(None);
    }
    if !input.starts_with('{') {
        return Err(format!(
            "json: cannot unmarshal {} into Go value of type {ty}",
            kind(input)
        ));
    }
    Ok(Some(input))
}

// The runtime prefixes typed handler errors, but not model-facing ResultErrors.
// Runtime-owned tools also wrap the decoder's complaint with `context`.
pub fn scalar<T: Default>(
    input: &str,
    ty: &str,
    key: &str,
    read: impl Fn(&str, &mut T, &str) -> Result<(), String>,
    context: &str,
) -> Result<T, String> {
    let decode = || -> Result<T, String> {
        let mut v = T::default();
        if input.len() > 64 * 1024 {
            return Err("argument size limit reached".into());
        }
        let input = normalize_json(input)?;
        if let Some(obj) = object(&input, ty)? {
            fields(obj, |k, val| {
                if field_matches(&k, key) {
                    read(val, &mut v, &field_path(ty, &k))
                } else {
                    Ok(())
                }
            })?;
        }
        Ok(v)
    };
    decode().map_err(|e| format!("Error calling tool: {context}{e}"))
}

/// Go names a struct field by its type without the package and the key as it was sent.
pub fn field_path(ty: &str, key: &str) -> String {
    let name = ty.rsplit_once('.').map_or("", |(_, name)| name);
    format!("Go struct field {name}.{key}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decoded_map_numbers_use_go_float64_spelling() {
        let v: serde_json::Value = serde_json::from_str(
            "[1.0,-0,9007199254740993,-1.2345678901234567e17,1e20,1e21,-1.23e-6,1e-7,5e-324]",
        )
        .unwrap();
        assert_eq!(encode_decoded(&v), "[1,-0,9007199254740992,-123456789012345660,100000000000000000000,1e+21,-0.00000123,1e-7,5e-324]");
    }

    #[test]
    fn syntax_errors_match_the_go_corpus() {
        let cases: Vec<(String, String)> =
            serde_json::from_str(include_str!("../testdata/go-json-errors.json")).unwrap();
        let mut unsupported = 0;
        for (input, expected) in &cases {
            match syntax_error(input) {
                Ok(message) => assert_eq!(message.unwrap_or_default(), *expected, "{input:?}"),
                Err(Unsupported) => {
                    assert!(!input.is_ascii() && !expected.is_empty(), "{input:?}");
                    unsupported += 1;
                }
            }
        }
        assert!(cases.len() > 500 && unsupported < 40, "{unsupported}");
        let deep = "[".repeat(MAX_JSON_DEPTH + 1);
        assert_eq!(syntax_error(&deep).unwrap().unwrap(), "exceeded max depth");
    }
}
