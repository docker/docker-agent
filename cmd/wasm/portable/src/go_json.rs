//! Go encoding/json conventions shared by typed builtin handlers.
use serde::{de::IgnoredAny, Serialize};
use std::{borrow::Cow, io};

pub const MAX_JSON_DEPTH: usize = 10_000;

// serde_json already matches encoding/json for quotes, backslashes and control characters.
struct GoFormatter;

impl serde_json::ser::Formatter for GoFormatter {
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
    let mut bytes = Vec::new();
    value
        .serialize(&mut serde_json::Serializer::with_formatter(
            &mut bytes,
            GoFormatter,
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

pub fn object<'a>(input: &'a str, pkg: &str, ty: &str) -> Result<Option<&'a str>, String> {
    if input.is_empty() {
        return Ok(None);
    }
    let input = input.trim_matches([' ', '\t', '\r', '\n']);
    serde_json::from_str::<IgnoredAny>(input).map_err(|e| {
        if e.is_eof() {
            "unexpected end of JSON input".into()
        } else {
            e.to_string()
        }
    })?;
    if input == "null" {
        return Ok(None);
    }
    if !input.starts_with('{') {
        return Err(format!(
            "json: cannot unmarshal {} into Go value of type {pkg}.{ty}",
            kind(input)
        ));
    }
    Ok(Some(input))
}

// The runtime prefixes typed handler errors, but not model-facing ResultErrors.
pub fn scalar<T: Default>(
    input: &str,
    pkg: &str,
    ty: &str,
    key: &str,
    read: impl Fn(&str, &mut T, &str) -> Result<(), String>,
) -> Result<T, String> {
    let decode = || -> Result<T, String> {
        let mut v = T::default();
        if input.len() > 64 * 1024 {
            return Err("argument size limit reached".into());
        }
        let input = normalize_json(input)?;
        if let Some(obj) = object(&input, pkg, ty)? {
            fields(obj, |k, val| {
                if field_matches(&k, key) {
                    read(val, &mut v, &format!("Go struct field {ty}.{key}"))
                } else {
                    Ok(())
                }
            })?;
        }
        Ok(v)
    };
    decode().map_err(|e| format!("Error calling tool: invalid arguments: {e}"))
}
