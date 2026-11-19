//! Project JS `chat.Message` history to the shapes the selected OpenAI protocol accepts.
use base64::{engine::general_purpose::STANDARD, Engine};
use serde::Deserialize;
use serde_json::{json, Value};
use std::collections::BTreeSet;

const MIB: usize = 1024 * 1024;
const MAX_ATTACHMENT: usize = 5 * MIB;
const MAX_TOTAL: usize = 16 * MIB;

#[derive(Clone, Copy, Default, Deserialize, PartialEq, Eq)]
#[serde(default, deny_unknown_fields)]
pub struct Capabilities {
    image: bool,
    pdf: bool,
    audio: bool,
}

#[derive(Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
enum Part {
    Text { text: String },
    ImageUrl { image_url: Image },
    Document { document: Document },
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Image {
    url: String,
    detail: Option<String>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Document {
    name: String,
    mime_type: String,
    size: Option<usize>,
    source: Source,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Source {
    inline_text: Option<String>,
    inline_data: Option<String>,
}

#[derive(Deserialize)]
struct Message {
    role: String,
    content: Option<String>,
    multi_content: Option<Vec<Part>>,
    reasoning_content: Option<String>,
    tool_call_id: Option<String>,
    tool_calls: Option<Vec<Value>>,
}

struct Profile {
    caps: Capabilities,
    responses: bool,
    legacy_image: bool,
    total: usize,
}

// The small build has no models.dev snapshot. Its runtime does recognize Claude
// names for modality stripping; only legacy image URLs bypass the converter's caps lookup.
fn claude(id: &str) -> bool {
    let lower = id.trim().to_ascii_lowercase();
    let mut segments = lower.rsplit('/');
    let model = segments.next().unwrap_or("");
    segments.next() != Some("openai")
        && (model.starts_with("claude-")
            || model.starts_with("anthropic.claude-")
            || model.split_once('.').is_some_and(|(region, id)| {
                !region.is_empty() && id.starts_with("anthropic.claude-")
            }))
}

impl Capabilities {
    pub fn projection(caps: Option<Self>, model: &str) -> (Self, bool) {
        (
            caps.unwrap_or_default(),
            caps.map_or_else(|| claude(model), |c| c.image),
        )
    }
}

pub fn history(
    messages: Vec<Value>,
    caps: Option<Capabilities>,
    model: &str,
    responses: bool,
    preserve_names: bool,
) -> Result<Vec<Value>, String> {
    if messages.len() > 4096 {
        return Err("too many history messages".into());
    }
    let (caps, legacy_image) = Capabilities::projection(caps, model);
    let mut profile = Profile {
        caps,
        legacy_image,
        responses,
        total: 0,
    };
    let mut out = Vec::with_capacity(messages.len());
    let mut deferred = Vec::new();
    let mut pending = BTreeSet::new();
    let mut count = 0;
    for value in messages {
        if let Some(parts) = value["multi_content"].as_array() {
            count += parts.len();
            if count > 1024 {
                return Err("too many history parts".into());
            }
        }
        if !value.is_object()
            || [
                "function_call",
                "audio",
                "attachments",
                "provider_state",
                "openai_response",
            ]
            .iter()
            .any(|key| !value[key].is_null())
        {
            return Err("unsupported history message or provider state".into());
        }
        let name = preserve_names
            .then(|| value["name"].as_str().map(str::to_owned))
            .flatten();
        let m: Message =
            serde_json::from_value(value).map_err(|e| format!("invalid history: {e}"))?;
        let role = m.role.as_str();
        if !matches!(role, "user" | "assistant" | "system" | "tool") {
            return Err(format!("unsupported history role {role:?}"));
        }
        let mut msg = json!({"role":role});
        if let Some(name) = name {
            msg["name"] = name.into();
        }
        if role == "tool" {
            let id = m.tool_call_id.ok_or("missing tool_call_id in history")?;
            if !pending.remove(&id) {
                return Err("unexpected tool result in history".into());
            }
            msg["tool_call_id"] = id.into();
        } else {
            if !pending.is_empty() {
                return Err("missing tool results in history".into());
            }
            out.append(&mut deferred);
        }
        if let Some(mut calls) = m.tool_calls {
            if role != "assistant" {
                return Err("only assistant history can have tool calls".into());
            }
            for call in &mut calls {
                let id = call["id"]
                    .as_str()
                    .filter(|s| !s.is_empty())
                    .ok_or("missing tool ID in history")?;
                let f = &call["function"];
                if call["type"] != "function"
                    || !pending.insert(id.to_owned())
                    || f["name"].as_str().is_none_or(str::is_empty)
                    || !f["arguments"].is_string()
                {
                    return Err("invalid historical function call".into());
                }
                *call = json!({"type":"function", "id":id,"function":{"name":f["name"],"arguments":f["arguments"]}});
            }
            if !calls.is_empty() {
                msg["tool_calls"] = calls.into();
            }
        }
        let text = m.content.unwrap_or_default();
        let mut parts = m.multi_content.unwrap_or_default();
        if role != "tool" {
            if parts.is_empty() {
                if text.trim().is_empty() && pending.is_empty() {
                    continue;
                }
            } else {
                parts.retain(|p| !matches!(p, Part::Text { text } if text.trim().is_empty()));
                if parts.is_empty() {
                    if !pending.is_empty() {
                        return Err("tool call with only blank history parts".into());
                    }
                    continue;
                }
            }
        }
        if role == "assistant" && !responses {
            if let Some(s) = m.reasoning_content.filter(|s| !s.trim().is_empty()) {
                msg["reasoning_content"] = s.into();
            }
        }
        profile.content(&mut msg, role, text, parts, &mut deferred)?;
        if msg.get("content").is_some() || !msg["tool_calls"].is_null() {
            out.push(msg);
        }
    }
    if !pending.is_empty() {
        return Err("missing tool results in history".into());
    }
    out.append(&mut deferred);
    Ok(out)
}

impl Profile {
    fn content(
        &mut self,
        msg: &mut Value,
        role: &str,
        text: String,
        parts: Vec<Part>,
        deferred: &mut Vec<Value>,
    ) -> Result<(), String> {
        let mut body = Vec::new();
        let mut attachments = Vec::new();
        let mut has_parts = false;
        let mut has_text = false;
        for part in parts {
            let is_text = matches!(&part, Part::Text { .. });
            has_text |= is_text;
            // None from the runtime strip means the part never reaches oaistream.
            let Some(converted) = self.part(part)? else {
                continue;
            };
            has_parts = true;
            if !converted.is_null() {
                if is_text || role == "user" || (role == "tool" && self.responses) {
                    body.push(converted);
                } else if role == "tool" {
                    attachments.push(converted);
                }
            }
        }
        if !attachments.is_empty() {
            let id = msg["tool_call_id"]
                .as_str()
                .expect("validated tool message");
            attachments.insert(0, json!({"type":"text","text":format!("Attached content from tool result (tool_call_id={id}):")}));
            deferred.push(json!({"role":"user", "content":attachments}));
        }
        if role == "tool" && self.responses {
            if !has_text && !text.is_empty() {
                body.insert(0, self.text(&text));
            }
            msg["content"] = match body.as_mut_slice() {
                [] => text.into(),
                [one] if one["type"] == "input_text" => one["text"].take(),
                _ => body.into(),
            };
        } else if role == "assistant" {
            if !has_parts
                && text.trim().is_empty()
                && msg["tool_calls"].is_null()
                && (self.responses || msg["reasoning_content"].is_null())
            {
                return Ok(());
            }
            if !self.responses && !body.is_empty() {
                msg["content"] = body.into();
            } else if (!has_parts || self.responses) && !text.is_empty() {
                msg["content"] = text.into();
            } else if msg["tool_calls"].is_null() {
                msg["content"] = "".into();
            }
        } else if has_parts && (role != "tool" || has_text) {
            msg["content"] = body.into();
        } else {
            msg["content"] = text.into();
        }
        Ok(())
    }

    fn text(&self, text: &str) -> Value {
        json!({"type":if self.responses { "input_text" } else { "text" }, "text":text})
    }

    fn image(&self, url: String, detail: Option<String>) -> Value {
        if self.responses {
            return json!({"type":"input_image", "image_url":url, "detail":detail.as_deref().unwrap_or("auto")});
        }
        let mut image = json!({"url":url});
        if let Some(detail) = detail {
            image["detail"] = detail.into();
        }
        json!({"type":"image_url", "image_url":image})
    }

    fn charge(&mut self, size: usize) -> Result<(), String> {
        if size > MAX_ATTACHMENT || size > MAX_TOTAL - self.total {
            return Err("history attachment size limit exceeded".into());
        }
        self.total += size;
        Ok(())
    }

    fn binary(&mut self, mime: &str, data: &str) -> Result<usize, String> {
        if data.len() > MAX_ATTACHMENT.min(MAX_TOTAL - self.total).div_ceil(3) * 4 {
            return Err("history attachment size limit exceeded".into());
        }
        let bytes = STANDARD
            .decode(data)
            .map_err(|_| "invalid base64 attachment")?;
        self.charge(bytes.len())?;
        let riff = |tag: &[u8]| bytes.starts_with(b"RIFF") && bytes.get(8..12) == Some(tag);
        let valid = match mime {
            "image/png" => bytes.starts_with(b"\x89PNG\r\n\x1a\n"),
            "image/jpeg" => bytes.starts_with(b"\xff\xd8\xff"),
            "image/gif" => bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a"),
            "image/webp" => riff(b"WEBP"),
            "application/pdf" => bytes.starts_with(b"%PDF-"),
            "audio/wav" | "audio/x-wav" | "audio/wave" | "audio/vnd.wave" => riff(b"WAVE"),
            "audio/mp3" | "audio/mpeg" => {
                bytes.starts_with(b"ID3")
                    || (bytes.first() == Some(&0xff)
                        && bytes.get(1).is_some_and(|b| b & 0xe0 == 0xe0))
            }
            _ => return Err("unsupported attachment MIME type".into()),
        };
        if !valid {
            return Err("attachment signature does not match MIME type".into());
        }
        Ok(bytes.len())
    }

    // None denotes a runtime-stripped part (fall back to message.content if all stripped).
    // Null denotes a part retained by the runtime but omitted by the provider converter.
    fn part(&mut self, part: Part) -> Result<Option<Value>, String> {
        match part {
            Part::Text { text } => Ok(Some(self.text(&text))),
            Part::ImageUrl { image_url } => self.legacy(image_url),
            Part::Document { document } => self.document(document),
        }
    }

    fn legacy(&mut self, Image { url, detail }: Image) -> Result<Option<Value>, String> {
        let detail = detail.filter(|s| !s.is_empty());
        if detail
            .as_deref()
            .is_some_and(|s| !matches!(s, "low" | "high" | "auto"))
        {
            return Err("invalid image detail".into());
        }
        if let Some(data) = url.strip_prefix("data:") {
            let (mime, payload) = data
                .split_once(";base64,")
                .filter(|(m, _)| m.starts_with("image/"))
                .ok_or("expected a base64 image data URL")?;
            self.binary(mime, payload)?;
        } else {
            if url.len() > 8192 || url.contains(['\\', '#']) || !url.is_ascii() {
                return Err("invalid image URL".into());
            }
            let uri = url.parse::<http::Uri>().map_err(|_| "invalid image URL")?;
            if uri.scheme_str() != Some("https")
                || uri.host().is_none_or(str::is_empty)
                || uri.authority().is_none_or(|a| a.as_str().contains('@'))
            {
                return Err("expected an HTTPS image URL without credentials".into());
            }
            self.charge(url.len())?;
        }
        Ok(self.legacy_image.then(|| self.image(url, detail)))
    }

    fn document(&mut self, doc: Document) -> Result<Option<Value>, String> {
        let Document {
            name,
            mime_type,
            size,
            source,
        } = doc;
        if matches!(name.trim(), "" | "." | "..")
            || name.len() > 255
            || !name.is_ascii()
            || name
                .bytes()
                .any(|b| b.is_ascii_control() || b == b'/' || b == b'\\')
        {
            return Err("invalid attachment display name".into());
        }
        if !valid_mime(&mime_type) {
            return Err("invalid attachment MIME type".into());
        }
        let data = match (source.inline_text, source.inline_data) {
            (Some(body), None) if !body.is_empty() => {
                self.charge(body.len())?;
                check_size(size, body.len())?;
                let wrapped = envelope(&name, &mime_type, &body);
                // Delimiter replacement can expand a short hostile string manyfold.
                self.charge(wrapped.len().saturating_sub(body.len()))?;
                return Ok(Some(self.text(&wrapped)));
            }
            (None, Some(data)) => data,
            _ => return Err("document needs one nonempty inline source".into()),
        };
        let mime = mime_type.to_ascii_lowercase();
        check_size(size, self.binary(&mime, &data)?)?;
        if mime.starts_with("image/") {
            if !self.caps.image {
                return Ok(self.legacy_image.then_some(Value::Null));
            }
            return Ok(Some(
                self.image(format!("data:{mime_type};base64,{data}"), None),
            ));
        }
        if mime == "application/pdf" {
            if !self.caps.pdf {
                return Ok(Some(Value::Null));
            }
            let file = format!("data:{mime_type};base64,{data}");
            return Ok(Some(if self.responses {
                json!({"type":"input_file","file_data":file,"filename":name})
            } else {
                json!({"type":"file", "file":{"file_data":file,"filename":name}})
            }));
        }
        if !self.caps.audio {
            return Ok(None);
        }
        if self.responses {
            return Ok(Some(Value::Null));
        }
        let format = if matches!(mime.as_str(), "audio/mp3" | "audio/mpeg") {
            "mp3"
        } else {
            "wav"
        };
        Ok(Some(
            json!({"type":"input_audio", "input_audio":{"data":data,"format":format}}),
        ))
    }
}

fn check_size(size: Option<usize>, actual: usize) -> Result<(), String> {
    if size.is_some_and(|s| s != 0 && s != actual) {
        Err("incorrect attachment size".into())
    } else {
        Ok(())
    }
}

fn valid_mime(mime: &str) -> bool {
    mime.len() <= 127
        && mime.split_once('/').is_some_and(|(ty, subtype)| {
            [ty, subtype].iter().all(|s| {
                s.as_bytes().first().is_some_and(u8::is_ascii_alphanumeric)
                    && s.bytes()
                        .all(|b| b.is_ascii_alphanumeric() || b"!#$&^_.+-".contains(&b))
            })
        })
}

fn envelope(name: &str, mime: &str, body: &str) -> String {
    let slug = format!("{name}-{mime}").to_ascii_lowercase();
    let slug = slug
        .split(|c: char| !c.is_ascii_alphanumeric())
        .filter(|s| !s.is_empty())
        .collect::<Vec<_>>()
        .join("-");
    let tag = format!("document-{slug}");
    let mut rest = body;
    let mut out = format!("<{tag}>\n");
    while let Some((prefix, tail)) = rest.split_once('<') {
        out.push_str(prefix);
        let name = tail.trim_start_matches(|c: char| c == '/' || c.is_ascii_whitespace());
        if name
            .get(..tag.len())
            .is_some_and(|s| s.eq_ignore_ascii_case(&tag))
        {
            // Defuse even nested/padded delimiters without interpreting other document markup.
            if let Some((_, suffix)) = name.split_once('>') {
                out.push_str("[docker-agent: envelope delimiter removed]");
                rest = suffix;
                continue;
            }
            out.push('<');
            rest = tail;
            break;
        }
        out.push('<');
        rest = tail;
    }
    out.push_str(rest);
    out.push_str("\n</");
    out.push_str(&tag);
    out.push('>');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn with_part(p: Value) -> Result<Vec<Value>, String> {
        history(
            vec![json!({"role":"user", "multi_content":[p]})],
            Some(Capabilities {
                image: true,
                audio: true,
                pdf: true,
            }),
            "test",
            false,
            false,
        )
    }

    fn document(mime: &str, source: Value) -> Value {
        json!({"type":"document", "document":{"name":"file", "mime_type":mime, "source":source}})
    }

    #[test]
    fn refuses_unsupported_roles_and_wire_shapes() {
        for m in [
            json!({"role":"developer","content":"rule"}),
            json!({"role":"user","content":[]}),
            json!({"role":"assistant","content":"ok","audio":{"id":"ref"}}),
        ] {
            assert!(history(vec![m], None, "test", false, false).is_err());
        }
        for p in [
            json!({"type":"file","file":{"file_id":"123"}}),
            json!({"type":"input_audio"}),
            json!({"type":"text","text":"ok","extra":true}),
        ] {
            assert!(with_part(p).is_err());
        }
    }

    #[test]
    fn rejects_invalid_documents_and_sizes_before_model_io() {
        let mut size = document("text/plain", json!({"inline_text":"é"}));
        size["document"]["size"] = 1.into();
        let mut name = document("text/plain", json!({"inline_text":"hi"}));
        name["document"]["name"] = "../note".into();
        for p in [
            size,
            name,
            document("text/plain", json!({"artifact_path":"/note"})),
            document(
                "text/plain",
                json!({"inline_data":"YQ==","inline_text":"a"}),
            ),
            document("image/png", json!({"inline_data":"!"})),
            document("image/png", json!({"inline_data":"JVBERi0xLjc="})),
            document("image/svg+xml", json!({"inline_data":"YQ=="})),
            document("text/plain\r\n", json!({"inline_text":"bad"})),
            document(
                "application/pdf",
                json!({"inline_data":"A".repeat(MAX_ATTACHMENT.div_ceil(3)*4+1)}),
            ),
        ] {
            assert!(with_part(p).is_err());
        }
        let p = json!({"type":"text", "text":"x"});
        assert!(history(
            vec![json!({"role":"user", "multi_content":vec![p; 1025]})],
            None,
            "test",
            false,
            false
        )
        .is_err());
        let mut profile = Profile {
            total: MAX_TOTAL - 1,
            caps: Capabilities::default(),
            responses: false,
            legacy_image: false,
        };
        assert!(profile.charge(1).is_ok());
        assert!(profile.charge(1).is_err());
    }

    #[test]
    fn legacy_urls_are_data_not_requests() {
        for url in [
            "file:///tmp/x.png",
            "data:image/svg+xml;base64,YQ==",
            "data:text/plain;base64,YQ==",
            "data:image/jpeg;base64,JUNK",
            "https://user:pass@example.org/x",
            "http://example.org/x",
            "https://example.org/a#fragment",
            "https://example.org\\a",
            "/local.png",
        ] {
            assert!(
                with_part(json!({"type":"image_url", "image_url":{"url":url}})).is_err(),
                "{url}"
            );
        }
        let url = "data:image/jpeg;base64,/9j/4A==";
        let p = json!({"type":"image_url","image_url":{"url":url,"detail":"high"}});
        assert_eq!(with_part(p.clone()).unwrap()[0]["content"][0], p);
        let mp3 = document("audio/mpeg", json!({"inline_data":"SUQz"}));
        assert_eq!(
            with_part(mp3).unwrap()[0]["content"][0],
            json!({"type":"input_audio", "input_audio":{"format":"mp3","data":"SUQz"}})
        );
    }

    #[test]
    fn defuses_delimiters_but_preserves_unrelated_markup() {
        let body = "<p>α</p><document-another </ DOCUMENT-NOTE-TXT-TEXT-PLAIN-extra x>>";
        assert_eq!(envelope("Note.txt", "text/plain", body), "<document-note-txt-text-plain>\n<p>α</p><document-another [docker-agent: envelope delimiter removed]>\n</document-note-txt-text-plain>");
    }
}
