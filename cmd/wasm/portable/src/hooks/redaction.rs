//! Port of github.com/docker/portcullis. Regenerate the pinned catalogue with
//! `sh cmd/wasm/portable/gen/redaction/generate.sh`.
use aho_corasick::{AhoCorasick, AhoCorasickBuilder};
use base64::{
    alphabet,
    engine::{DecodePaddingMode, GeneralPurpose, GeneralPurposeConfig},
    Engine,
};
use regex::Regex;
use serde::Deserialize;
use serde_json::Value;
use std::{
    collections::BTreeMap,
    sync::{LazyLock, OnceLock},
};

#[derive(Deserialize)]
struct Rule {
    pattern: String,
    #[serde(default)]
    keywords: Vec<String>,
    #[serde(default)]
    case_sensitive: bool,
    validator: Option<Validator>,
    #[serde(skip)]
    compiled: OnceLock<Regex>,
}

#[derive(Deserialize)]
struct Catalog {
    rules: Vec<Rule>,
    iban_lengths: BTreeMap<String, usize>,
}

struct Scanner {
    catalog: Catalog,
    keywords: AhoCorasick,
    subscriptions: Vec<Vec<usize>>,
}

static SCANNER: LazyLock<Scanner> = LazyLock::new(|| {
    let catalog: Catalog =
        serde_json::from_str(include_str!("rules.json")).expect("redaction catalog");
    let mut index = BTreeMap::<String, Vec<usize>>::new();
    for (i, rule) in catalog.rules.iter().enumerate() {
        for k in &rule.keywords {
            index.entry(k.to_ascii_lowercase()).or_default().push(i);
        }
    }
    let keywords = AhoCorasickBuilder::new()
        .ascii_case_insensitive(true)
        .build(index.keys())
        .expect("redaction keywords");
    Scanner {
        catalog,
        keywords,
        subscriptions: index.into_values().collect(),
    }
});

impl Rule {
    fn regex(&self) -> &Regex {
        self.compiled
            .get_or_init(|| Regex::new(&self.pattern).expect("generated Portcullis expression"))
    }
}

/// None means leave the original bytes untouched. Resource limits fail closed.
pub fn redact(text: &str) -> Result<Option<String>, String> {
    if text.is_empty() {
        return Ok(None);
    }
    if text.len() > 1024 * 1024 {
        return Err("redaction input size limit reached".into());
    }
    let scanner = &*SCANNER;
    let mut enabled: Vec<_> = scanner
        .catalog
        .rules
        .iter()
        .map(|r| r.keywords.is_empty())
        .collect();
    for hit in scanner.keywords.find_overlapping_iter(text) {
        for &i in &scanner.subscriptions[hit.pattern().as_usize()] {
            enabled[i] = true;
        }
    }
    let mut spans = Vec::new();
    for (rule, enabled) in scanner.catalog.rules.iter().zip(enabled) {
        if !enabled || (rule.case_sensitive && !rule.keywords.iter().any(|s| text.contains(s))) {
            continue;
        }
        for caps in rule.regex().captures_iter(text) {
            let span = caps.name("secret").unwrap_or_else(|| caps.get(0).unwrap());
            if rule
                .validator
                .is_some_and(|v| !v.accepts(span.as_str(), &scanner.catalog))
            {
                continue;
            }
            if spans.len() == 16 * 1024 {
                return Err("redaction match limit reached".into());
            }
            spans.push((span.start(), span.end()));
        }
    }
    if spans.is_empty() {
        return Ok(None);
    }
    spans.sort_unstable();
    let mut result = String::new();
    let mut end = 0;
    for (start, stop) in spans {
        if start >= end {
            result.push_str(&text[end..start]);
            result.push_str("[REDACTED]");
        }
        end = end.max(stop);
    }
    result.push_str(&text[end..]);
    Ok((result != text).then_some(result))
}

#[derive(Deserialize, Clone, Copy)]
enum Validator {
    #[serde(rename = "AWSAccessKeyID")]
    AwsAccessKeyId,
    #[serde(rename = "AWSBedrockLongLivedKey")]
    AwsBedrockLongLivedKey,
    AgeSecretKey,
    AzureStorageAccountKey,
    CloudflareAPIKey,
    DiscordWebhookURL,
    DockerConfigSecret,
    GitHubChecksum,
    GitHubStatelessToken,
    #[serde(rename = "IBAN")]
    Iban,
    #[serde(rename = "JWT")]
    Jwt,
    Luhn,
    #[serde(rename = "USSSN")]
    UsSsn,
}

const STANDARD: GeneralPurpose = GeneralPurpose::new(
    &alphabet::STANDARD,
    GeneralPurposeConfig::new().with_decode_allow_trailing_bits(true),
);
const URL_SAFE: GeneralPurpose = GeneralPurpose::new(
    &alphabet::URL_SAFE,
    GeneralPurposeConfig::new()
        .with_decode_allow_trailing_bits(true)
        .with_decode_padding_mode(DecodePaddingMode::RequireNone),
);

impl Validator {
    // The catalogue regex already enforces the alphabet, length, and prefix of its match.
    fn accepts(self, text: &str, catalog: &Catalog) -> bool {
        match self {
            Self::AwsAccessKeyId => {
                let digit = |b| {
                    b"ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
                        .iter()
                        .position(|&c| c == b)
                        .map(|n| n as i64)
                };
                let Some(n) = text.as_bytes()[4..12]
                    .iter()
                    .try_fold(0, |n, &b| Some(n * 32 + digit(b)?))
                else {
                    return false;
                };
                let account =
                    (n - (16 << 35)) * 2 + i64::from(digit(text.as_bytes()[12]) >= Some(16));
                (0..=999_999_999_999).contains(&account)
            }
            Self::AwsBedrockLongLivedKey => STANDARD
                .decode(&text[4..])
                .is_ok_and(|v| v.starts_with(b"BedrockAPIKey-")),
            Self::AzureStorageAccountKey => STANDARD.decode(text).is_ok_and(|v| v.len() == 64),
            Self::CloudflareAPIKey => !text.contains(['-', '_']),
            // Conservatively scrub even future snowflakes; the portable module has no clock.
            Self::DiscordWebhookURL => text
                .rsplit('/')
                .nth(1)
                .is_some_and(|id| id.parse::<u64>().is_ok()),
            Self::AgeSecretKey => valid_age(text),
            Self::GitHubChecksum => {
                let body = text.strip_prefix("github_pat_").unwrap_or(&text[4..]);
                let (random, check) = body.split_at(body.len() - 6);
                let mut crc = crc32fast::hash(random.as_bytes());
                check.bytes().rev().all(|b| {
                    let digit = b"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
                        [(crc % 62) as usize];
                    crc /= 62;
                    digit == b
                })
            }
            Self::GitHubStatelessToken => valid_jwt(&text[4..]),
            Self::Jwt => valid_jwt(text),
            Self::DockerConfigSecret => STANDARD
                .decode(text)
                .is_ok_and(|b| decoded_object(&b, valid_docker_config)),
            Self::UsSsn => {
                let digits: Vec<_> = text.bytes().filter(u8::is_ascii_digit).collect();
                digits.len() == 9
                    && digits[0] < b'9'
                    && !matches!(&digits[..3], b"000" | b"666")
                    && &digits[3..5] != b"00"
                    && &digits[5..] != b"0000"
            }
            Self::Luhn => {
                let mut sum = 0;
                for (i, b) in text.bytes().rev().filter(u8::is_ascii_digit).enumerate() {
                    let mut d = b - b'0';
                    if i % 2 == 1 {
                        d *= 2;
                        if d > 9 {
                            d -= 9;
                        }
                    }
                    sum += u32::from(d);
                }
                sum % 10 == 0
            }
            Self::Iban => {
                let s = text.replace(' ', "").to_uppercase();
                if !s.is_ascii() || catalog.iban_lengths.get(&s[..2]) != Some(&s.len()) {
                    return false;
                }
                s[4..].bytes().chain(s[..4].bytes()).fold(0u32, |n, b| {
                    if b.is_ascii_digit() {
                        (n * 10 + u32::from(b - b'0')) % 97
                    } else {
                        (n * 100 + u32::from(b - b'A' + 10)) % 97
                    }
                }) == 1
            }
        }
    }
}

// Go's JSON decoder accepts, for example, unpaired surrogates and more nesting
// than serde_json. If we cannot inspect decoded JSON reliably, redact it.
fn decoded_object(bytes: &[u8], valid: impl FnOnce(&Value) -> bool) -> bool {
    let bytes = bytes.trim_ascii();
    if !bytes.starts_with(b"{") || !bytes.ends_with(b"}") {
        return false;
    }
    serde_json::from_slice(bytes).map_or(true, |v| {
        // Go can merge duplicate object fields. Don't unmask a candidate based
        // on a lossy parse (including duplicate keys hidden by JSON escapes).
        valid(&v) || !bytes.is_ascii() || serde_json::to_vec(&v).expect("JSON value") != bytes
    })
}
fn fields<'a>(v: &'a Value, name: &'a str) -> impl Iterator<Item = &'a Value> {
    v.as_object()
        .into_iter()
        .flatten()
        .filter_map(move |(k, v)| k.eq_ignore_ascii_case(name).then_some(v))
}
fn valid_docker_config(cfg: &Value) -> bool {
    fields(cfg, "auths")
        .filter_map(Value::as_object)
        .any(|auths| {
            !auths.is_empty()
                && auths.iter().all(|(key, v)| {
                    !key.is_empty()
                        && fields(v, "auth").filter_map(Value::as_str).any(|auth| {
                            !auth.is_empty()
                                && STANDARD.decode(auth.replace(['\r', '\n'], "")).is_ok()
                        })
                })
        })
}
fn valid_jwt(text: &str) -> bool {
    let mut parts = text.split('.');
    let (Ok(h), Some(p)) = (
        URL_SAFE.decode(parts.next().unwrap_or_default()),
        parts.next(),
    ) else {
        return false;
    };
    let Ok(p) = URL_SAFE.decode(p) else {
        return false;
    };
    decoded_object(&h, |v| {
        fields(v, "alg")
            .filter_map(Value::as_str)
            .any(|alg| !alg.is_empty() && !alg.eq_ignore_ascii_case("none"))
    }) && decoded_object(&p, |v| v.as_object().is_some_and(|o| !o.is_empty()))
}
fn valid_age(s: &str) -> bool {
    let hrp = b"age-secret-key-";
    let alphabet = b"QPZRY9X8GF2TVDW0S3JN54KHCE6MUA7L";
    let digits: Vec<_> = s[16..]
        .bytes()
        .map(|b| alphabet.iter().position(|&c| c == b).unwrap() as u8)
        .collect();
    // 32 bytes leave four zero padding bits.
    if digits[51] & 15 != 0 {
        return false;
    }
    let mut chk = 1u32;
    for value in hrp
        .iter()
        .map(|b| b >> 5)
        .chain([0])
        .chain(hrp.iter().map(|b| b & 31))
        .chain(digits)
    {
        let top = chk >> 25;
        chk = ((chk & 0x1ffffff) << 5) ^ u32::from(value);
        for (i, g) in [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3]
            .iter()
            .enumerate()
        {
            if top & (1 << i) != 0 {
                chk ^= g;
            }
        }
    }
    chk == 1
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn every_generated_expression_compiles() {
        assert!(SCANNER.catalog.rules.len() > 200);
        for rule in &SCANNER.catalog.rules {
            rule.regex();
        }
    }
    #[test]
    fn resource_caps_fail_closed() {
        assert!(redact(&"x".repeat(1024 * 1024 + 1)).is_err());
        assert!(redact(&format!("glpat-{} ", "a".repeat(20)).repeat(16 * 1024 + 1)).is_err());
    }

    #[test]
    fn checksums_context_and_overlaps() {
        let github = format!("ghp_{}0uCPlr", "A".repeat(30));
        let invalid = format!("ghp_{}", "A".repeat(36));
        let source = format!("{github} {invalid} postgres://u:passtext@db 4242 4242 4242 4242\nDE89 3704 0044 0532 0130 00");
        let clean =
            format!("[REDACTED] {invalid} postgres://u:[REDACTED]@db [REDACTED]\n[REDACTED]");
        assert_eq!(redact(&source).unwrap().unwrap(), clean);
        assert_eq!(redact(&clean).unwrap(), None);
        let token = format!("glpat-{}.aa1234567", "a".repeat(50));
        assert_eq!(
            redact(&format!("{token}{github}")).unwrap().unwrap(),
            "[REDACTED][REDACTED]"
        );
    }
}
