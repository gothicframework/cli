(function() {
    window.__gothic_dev = true;

    let src = window.gothicframework_reloadSrc
        || new EventSource("/_gothicframework/reload/events");

    // ── Dev status badge ──
    // Single floating element shared across building/error states.
    // Fixed position bottom-left, non-interactive, highest z-index
    // to survive user CSS stacking contexts.

    function getBadge() {
        let el = document.getElementById("__gothic_badge");
        if (!el && document.body) {
            el = document.createElement("div");
            el.id = "__gothic_badge";
            document.body.appendChild(el);
        }
        return el || null;
    }

    function showBadge(state, text) {
        let el = getBadge();
        if (!el) return;

        let dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
        let reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

        const base = {
            position: "fixed",
            bottom: "12px",
            left: "12px",
            zIndex: "2147483647",
            pointerEvents: "none",
            fontFamily: "system-ui, -apple-system, sans-serif",
            fontSize: "12px",
            lineHeight: "1.4",
            borderRadius: "6px",
            transition: reduceMotion ? "none" : "opacity 0.15s ease",
        };

        if (state === "building") {
            Object.assign(el.style, base, {
                background: dark ? "rgba(30, 64, 175, 0.9)" : "rgba(219, 234, 254, 0.95)",
                color: dark ? "#bfdbfe" : "#1e40af",
                padding: "6px 10px",
                maxWidth: "320px",
                border: "1px solid " + (dark ? "rgba(59, 130, 246, 0.4)" : "rgba(147, 197, 253, 0.6)"),
            });
            el.textContent = "Building: " + text;
        } else if (state === "failed") {
            Object.assign(el.style, base, {
                background: "rgba(185, 28, 28, 0.95)",
                color: "#fecaca",
                padding: "8px 12px",
                maxWidth: "420px",
                border: "1px solid rgba(248, 113, 113, 0.4)",
                whiteSpace: "pre-wrap",
                wordBreak: "break-word",
            });
            // Truncate long compiler errors to keep the badge compact.
            if (text.length > 300) {
                text = text.substring(0, 300) + "...";
            }
            el.textContent = "Build error: " + text;
        }
    }

    function hideBadge() {
        let el = document.getElementById("__gothic_badge");
        if (el && el.parentNode) el.parentNode.removeChild(el);
    }

    // building, WASM compilation started (payload: project-relative file path)
    // The paint reload navigates away while the compile is still running, so the
    // badge is re-established from the server's replayed state on reconnect.
    src.addEventListener("building", function(ev) {
        showBadge("building", ev.data || "Compiling...");
    });

    // builddone, the WASM stage settled with no error. Sent whether or not any
    // unit was rebuilt, and always before the reload that carries a fresh
    // binary, so the swapped-in document starts clean.
    src.addEventListener("builddone", function() {
        hideBadge();
    });

    // builderror, compilation failed (payload: error message)
    src.addEventListener("builderror", function(ev) {
        showBadge("failed", ev.data || "Unknown build error");
    });

    // message, "reload" (fresh binary ready, navigate to the new document)
    //           or keepalive "ping" (filtered by data guard below)
    src.addEventListener("message", function(ev) {
        if (!ev || ev.data !== "reload") return;

        // Close SSE before navigating so the server-side slot is freed immediately.
        src.close();
        window.gothicframework_reloadSrc = null;

        // A navigation, not a document swap: the browser holds the current paint
        // until the new document has its stylesheet, so the page never appears
        // unstyled. Writing into document.open destroyed the document and painted
        // whatever had parsed, ahead of the CSS, which showed as a one-frame flash.
        window.location.reload();
    });

    window.gothicframework_reloadSrc = src;
    window.onbeforeunload = function() { src.close(); };
})();
// ── Dev bus observer ─────────────────────────────────────────────────────────
//
// Taps the WASM topic data-plane (window.__gothic_topic.set/.setBytes +
// window.__gothicDispatchAsync, installed by gothic-core.js) and the control
// plane (document.dispatchEvent), decodes the binary frames against the
// gothic-wire/1 schema descriptors the runtime deposits on
// window.__gothicSchemas, and POSTs decoded records in small batches to the
// dev proxy at /_gothicframework/reload/bus (a mutex-protected ring buffer,
// readable via GET ?last=N).
//
// Pure decode logic is exported as window.__gothicDevDecode so tests can drive
// it without a live page. Everything here is gated by window.__gothic_dev —
// without the dev script none of it runs and the page behaves identically.
//
// The shared pool keeps only the LATEST frame per key, so exact event↔payload
// pairing is done here: every set/setBytes deposit is copied immediately (the
// pool reuses its backing arrays) and queued FIFO per event name; each
// __gothicDispatchAsync(name) shifts that queue and emits one record. A
// dispatch with no queued frame is never recorded — no invented record from
// pings or replayed control events.
(function() {
    var FLUSH_MS = 250;
    var FLUSH_MAX = 50;    // records per POST
    var QUEUE_CAP = 400;   // drop-on-backpressure: sacrifice the oldest record
    var RAW_HEX_CAP = 512; // hex chars kept per undecodable frame
    var STR_CAP = 1024;    // chars kept per decoded string value

    var queue = [];
    var flushTimer = null;
    var sending = false;
    var schemas = null;          // parsed keyName → descriptor cache
    var fifo = Object.create(null); // event name → [{t, bytes}]
    var nextSeq = 0;             // per-page record sequence (collision-safe dedup key)

    // ── Decode helpers (pure; also exported on window.__gothicDevDecode) ──

    var OP_WIDTHS = { U8: 1, U16: 2, U32: 4, U64: 8, I32: 4, I64: 8, F32: 4, F64: 8, Bool: 1 };
    var HEX = "0123456789abcdef";
    var textDecoder = null;
    try { textDecoder = new TextDecoder("utf-8"); } catch (e) { textDecoder = null; }

    // hexDecode(bytes) → Uint8Array, or null when not hex-shaped.
    function hexDecode(bytes) {
        if (!bytes || bytes.length < 2 || bytes.length % 2 !== 0) return null;
        var out = new Uint8Array(bytes.length / 2);
        for (var i = 0; i < bytes.length; i += 2) {
            var h = HEX.indexOf(String.fromCharCode(bytes[i]));
            var l = HEX.indexOf(String.fromCharCode(bytes[i + 1]));
            if (h < 0 || l < 0) return null;
            out[i / 2] = (h << 4) | l;
        }
        return out;
    }

    function hexEncode(bytes) {
        var s = "";
        for (var i = 0; i < bytes.length; i++) {
            s += HEX[bytes[i] >> 4] + HEX[bytes[i] & 15];
            if (s.length >= RAW_HEX_CAP) break;
        }
        return s;
    }

    // parseDescriptor parses one gothic-wire/1 descriptor:
    //   gothic-wire/1\n<Struct>[ key=<key>]\n  <Field>=<op>,<op>,...\n...
    // Returns {structName, key, fields: [{name, ops: [tokens]}]} or null.
    function parseDescriptor(desc) {
        if (typeof desc !== "string") return null;
        var lines = desc.split("\n");
        if (lines.length < 2 || lines[0].indexOf("gothic-wire/1") !== 0) return null;
        var head = lines[1].trim();
        var structName = head, key = "";
        var keyIdx = head.indexOf(" key=");
        if (keyIdx > 0) {
            structName = head.substring(0, keyIdx).trim();
            key = head.substring(keyIdx + 5).trim();
        }
        if (!structName) return null;
        var fields = [];
        for (var i = 2; i < lines.length; i++) {
            var line = lines[i].trim();
            if (!line) continue;
            var eq = line.indexOf("=");
            if (eq <= 0) continue;
            var name = line.substring(0, eq).trim();
            var ops = [];
            var tokens = line.substring(eq + 1).trim().split(",");
            for (var j = 0; j < tokens.length; j++) {
                if (tokens[j]) ops.push(tokens[j]);
            }
            fields.push({ name: name, ops: ops });
        }
        return { structName: structName, key: key, fields: fields };
    }

    // parseSchemas(store) reduces window.__gothicSchemas (schemaID →
    // {id, key, descriptor}) into a key → parsed-descriptor registry. All
    // records for one topic key carry the same descriptor; the first wins.
    function parseSchemas(store) {
        var byKey = {};
        if (!store || typeof store !== "object") return byKey;
        for (var id in store) {
            var rec = store[id];
            if (!rec || !rec.descriptor || !rec.key || byKey[rec.key]) continue;
            var parsed = parseDescriptor(rec.descriptor);
            if (parsed) byKey[rec.key] = parsed;
        }
        return byKey;
    }

    // parseEventName(name) → {kind, topic, field} or null when not bus-shaped.
    // Known shapes:
    //   gothic:topic:<key>[:<field>]      gothic:topic-req:<key>[:<field>]
    //   gothic:topic-online:<key>         gothic:topic-ping:<key>
    //   gothic:durable:<key>[:<field>]    gothic:durable-req:<key>[:<field>]
    // Keys never contain a colon.
    var EVENT_PREFIXES = [
        "gothic:topic-online:", "gothic:topic-req:", "gothic:topic-ping:",
        "gothic:topic:", "gothic:durable-req:", "gothic:durable:"
    ];

    function parseEventName(name) {
        if (typeof name !== "string") return null;
        for (var i = 0; i < EVENT_PREFIXES.length; i++) {
            if (name.indexOf(EVENT_PREFIXES[i]) === 0) {
                var rest = name.substring(EVENT_PREFIXES[i].length);
                var kind = name.indexOf("gothic:durable") === 0 ? "durable" : "topic";
                var topic = rest, field = null;
                var colon = rest.indexOf(":");
                if (colon >= 0) {
                    topic = rest.substring(0, colon);
                    field = rest.substring(colon + 1) || null;
                }
                if (!topic) return null;
                return { kind: kind, topic: topic, field: field };
            }
        }
        return null;
    }

    // Reader: strict little-endian positional reads with a sticky error flag
    // (mirrors the Go Decoder's sticky Err — one throw, every later read fails).
    function makeReader(bytes) {
        var pos = 0, broken = false;
        function need(n) {
            if (broken || pos + n > bytes.length) { broken = true; throw "underflow"; }
        }
        return {
            ok: function() { return !broken; },
            u8: function() { need(1); return bytes[pos++]; },
            u16: function() { need(2); var v = bytes[pos] | (bytes[pos + 1] << 8); pos += 2; return v; },
            u32: function() {
                need(4);
                var v = (bytes[pos] | (bytes[pos + 1] << 8) | (bytes[pos + 2] << 16) | (bytes[pos + 3] << 24)) >>> 0;
                pos += 4; return v;
            },
            u64: function() {
                need(8);
                // UI-state payloads stay under 2^53, where Number is exact.
                var lo = this.u32(), hi = this.u32();
                return lo + hi * 4294967296;
            },
            i32: function() {
                var v = this.u32();
                return v >= 0x80000000 ? v - 4294967296 : v;
            },
            i64: function() {
                var v = this.u64();
                return v >= 9223372036854775808 ? v - 18446744073709551616 : v;
            },
            f32: function() {
                need(4);
                var v = new DataView(bytes.buffer, bytes.byteOffset + pos, 4).getFloat32(0, true);
                pos += 4; return v;
            },
            f64: function() {
                need(8);
                var v = new DataView(bytes.buffer, bytes.byteOffset + pos, 8).getFloat64(0, true);
                pos += 8; return v;
            },
            bool: function() { return this.u8() !== 0; },
            fixed: function(n) { need(n); var s = bytes.subarray(pos, pos + n); pos += n; return s; },
            u32len: function() {
                var n = this.u32();
                if (n > bytes.length) { broken = true; throw "underflow"; }
                var s = bytes.subarray(pos, pos + n); pos += n; return s;
            }
        };
    }

    // decodeOps(r, ops, structs) reads ONE field's value using its descriptor
    // op tokens. Two structural notes, both derived from what the CLI emits
    // (wireOpRe in cli/internal/build/wasm_schema.go):
    //
    // 1. Pointer fields emit the nil tag through BOTH branches, so the
    //    descriptor lists the U8 tag token twice ("U8,U8,<valueOps>"). The wire
    //    carries exactly ONE tag byte: a run of k≥2 identical U8 tokens consumes
    //    k-1 bytes — the tag, then (k-2) genuine U8 bytes present only when
    //    tag = 1. Tag 0 → the value ops after the run decode as nothing (null).
    // 2. A U32 followed by further tokens is a length/count prefix (slice or
    //    map) — a lone U32 is a scalar. The remaining tokens are the repeating
    //    group, one pass per element: this decodes slices AND maps (a map's
    //    per-entry body is exactly keyOps+valOps concatenated) without having
    //    to know whether the type was []T or map[K]V — the bytes are identical.
    // struct:Name recurses through the parsed schema registry; an unknown
    // struct or any misparse aborts (caller falls back to raw hex).
    function decodeOps(ops, r, structs) {
        // Nil-tag run: k≥2 identical U8 tokens = one tag byte (both branch
        // literals collapse to one wire byte), then (k-2) genuine U8 value
        // slots + the tail ops, all present only when the tag says so.
        if (ops.length >= 2 && ops[0] === "U8" && ops[1] === "U8") {
            var run = 2;
            while (run < ops.length && ops[run] === "U8") run++;
            // Underflow here (truncated frame) throws out of decodeOps —
            // caught as null by the caller, which falls back to raw hex.
            var tag = 0;
            try { tag = r.u8(); } catch (e) { return null; }
            if (!r.ok()) return null;
            if (!tag) {
                // Run slots + tail are the untaken branch: no bytes on the wire.
                return null;
            }
            // Tag set: (run-2) inline U8 value bytes for pointer-to-U8 shapes,
            // then the tail ops.
            var inline = null;
            try {
                for (var k = 2; k < run; k++) { inline = r.u8(); }
            } catch (e) { return null; }
            if (!r.ok()) return null;
            if (ops.length > run) return decodeTail(ops.slice(run), r, structs);
            // Pointer-to-U8 with no tail: the last inline byte IS the value.
            return inline;
        }
        if (ops[0] === "U32" && ops.length > 1) {
            var count = r.u32();
            if (!r.ok()) return null;
            var items = [];
            for (var i = 0; i < count && r.ok(); i++) {
                var item = decodeTail(ops.slice(1), r, structs);
                if (!r.ok()) return null;
                items.push(item);
            }
            return items;
        }
        return decodeTail(ops, r, structs);
    }

    // decodeTail decodes the remaining tokens of one loop iteration (or a fully
    // consumed scalar). One op token = one wire element; the returned value is
    // the last element for single-op lists, or an array for multi-op bodies
    // (tuple shape: map entries, pointer tails).
    function decodeTail(tokens, r, structs) {
        var values = [];
        for (var i = 0; i < tokens.length; i++) {
            var tok = tokens[i];
            if (tok === "skip") continue;
            try {
                if (tok === "U8") values.push(r.u8());
                else if (tok === "U16") values.push(r.u16());
                else if (tok === "U32") values.push(r.u32());
                else if (tok === "U64") values.push(r.u64());
                else if (tok === "I32") values.push(r.i32());
                else if (tok === "I64") values.push(r.i64());
                else if (tok === "F32") values.push(r.f32());
                else if (tok === "F64") values.push(r.f64());
                else if (tok === "Bool") values.push(r.bool());
                else if (tok === "String" || tok === "Bytes") {
                    var raw = r.u32len();
                    if (!r.ok()) return null;
                    if (tok === "Bytes") { values.push(bytesToB64(raw)); }
                    else { values.push(decodeUTF8(raw)); }
                }
                else if (tok.indexOf("struct:") === 0) {
                    var sd = structs[tok.substring(7)];
                    if (!sd) return null;
                    var obj = {};
                    for (var f = 0; f < sd.fields.length && r.ok(); f++) {
                        var v = decodeOps(sd.fields[f].ops, r, structs);
                        if (!r.ok()) return null;
                        obj[sd.fields[f].name] = v;
                    }
                    values.push(obj);
                }
                else { return null; } // unknown op token
            } catch (e) {
                return null;
            }
            if (!r.ok()) return null;
        }
        if (values.length === 1) return values[0];
        return values;
    }

    function decodeUTF8(bytes) {
        var s = textDecoder ? textDecoder.decode(bytes) : String.fromCharCode.apply(null, bytes);
        if (s.length > STR_CAP) s = s.substring(0, STR_CAP) + "…(" + bytes.length + " chars)";
        return s;
    }

    // bytesToB64: binary payload playground — keep Bytes fields printable.
    function bytesToB64(bytes) {
        var s = "";
        for (var i = 0; i < bytes.length && i < 96; i++) s += String.fromCharCode(bytes[i]);
        return s;
    }

    // The schema registry: lazily rebuilt when records appear on
    // window.__gothicSchemas (populated during component registration, which
    // can happen before OR after the first dispatch). A growing registry needs
    // a re-parse; detect by keeping the record set size we last parsed.
    var parsedSchemaCount = -1;
    function schemas$() {
        var store = window.__gothicSchemas;
        var count = store ? Object.keys(store).length : 0;
        if (schemas === null || count !== parsedSchemaCount) {
            schemas = parseSchemas(store);
            parsedSchemaCount = count;
        }
        return schemas;
    }

    // event name → parsed descriptor for its topic key.
    function schemaFor(topic) {
        return schemas$()[topic] || null;
    }

    // decodePayload(event, bytes) → decoded object, or {raw_hex, schema} when
    // the frame can't be interpreted (unknown struct, bad descriptor, odd
    // bytes), or null when there are no bytes to read.
    //
    // Two frame shapes ride the bus:
    //   - Per-field frames are raw binary: [WireVersion 0x01][LE typed fields],
    //     one field's value on gothic:{topic|durable}[-req]:<key>:<field>.
    //   - Whole-key frames (gothic:{topic,topic-req,topic-online}:<key>) are
    //     hex-ASCII (BinaryKey HexEncode legs) — sniff the leading "01" chars.
    function decodePayload(eventName, bytes) {
        if (!bytes || !bytes.length) return null;
        var parts = parseEventName(eventName);
        if (!parts) return null;
        var sd = schemaFor(parts.topic);
        if (!sd) return { raw_hex: hexEncode(bytes), schema: null };

        if (parts.field) {
            // Per-field frame: one field's value at field-ops.
            if (bytes[0] !== 0x01) return { raw_hex: hexEncode(bytes), schema: sd.structName };
            var fieldOps = null;
            for (var i = 0; i < sd.fields.length; i++) {
                if (sd.fields[i].name === parts.field) { fieldOps = sd.fields[i].ops; break; }
            }
            if (!fieldOps || !fieldOps.length) return { raw_hex: hexEncode(bytes), schema: sd.structName };
            var r = makeReader(bytes);
            try { r.u8(); } catch (e) { return { raw_hex: hexEncode(bytes), schema: sd.structName }; }
            if (!r.ok()) return { raw_hex: hexEncode(bytes), schema: sd.structName };
            // decodeOps may throw on a truncated frame (reader underflow):
            // caught here so the frame degrades to the raw_hex fallback
            // instead of the record being dropped.
            var value = null;
            try { value = decodeOps(fieldOps, r, structs()); } catch (e) { value = null; }
            if (!r.ok() || value === null) return { raw_hex: hexEncode(bytes), schema: sd.structName };
            var one = {};
            one[parts.field] = value;
            return one;
        }
        // Whole-key frame: hex-ASCII transport on the BinaryKey legs (the frame
        // begins with the WireVersion byte rendered as the chars "01" — the
        // rule from the codec's hex transport). Hex-decode, then decode the
        // FULL struct field-by-field.
        var frame = hexDecode(bytes);
        if (!frame || frame[0] !== 0x01) return { raw_hex: hexEncode(bytes), schema: sd.structName };
        var rr = makeReader(frame);
        try {
            rr.u8();
            var obj = {};
            for (var fi = 0; fi < sd.fields.length && rr.ok(); fi++) {
                var v = decodeOps(sd.fields[fi].ops, rr, structs());
                if (!rr.ok()) return { raw_hex: hexEncode(bytes), schema: sd.structName };
                obj[sd.fields[fi].name] = v;
            }
            return obj;
        } catch (e) {
            return { raw_hex: hexEncode(bytes), schema: sd.structName };
        }
    }

    // structs() is the struct:Name lookup. The store registers TOPIC structs
    // (by key); nested helper structs (Item, D1, …) are NOT registered, so
    // frames containing them fall back to raw hex — build the struct-name map
    // from the same parsed registry.
    function structs() {
        var reg = schemas$();
        var byName = {};
        for (var k in reg) {
            var sd = reg[k];
            if (sd && sd.structName && !byName[sd.structName]) byName[sd.structName] = sd;
        }
        return byName;
    }

    // ── Record emission ──

    function nowMs() { return Date.now(); }

    function enqueue(rec) {
        if (queue.length >= QUEUE_CAP) queue.shift(); // drop-on-backpressure
        queue.push(rec);
    }

    function flush() {
        flushTimer = null;
        if (!queue.length) return;
        if (sending) { armFlush(); return; }
        var batch = queue.slice(0, FLUSH_MAX);
        queue = queue.slice(FLUSH_MAX);
        sending = true;
        fetch("/_gothicframework/reload/bus", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ records: batch })
        }).then(function() {
            sending = false;
            if (queue.length) armFlush();
        }, function() {
            sending = false;   // swallowed: never block the page on a failed POST
            if (queue.length) armFlush();
        });
    }

    function armFlush() {
        if (!flushTimer) flushTimer = setTimeout(function() { flushTimer = null; flush(); }, FLUSH_MS);
    }

    function emitRecord(rec) {
        rec.t = nowMs();
        rec.seq = ++nextSeq;
        enqueue(rec);
        if (!flushTimer && !sending) armFlush();
    }

    function emitBusRecord(eventName, dir, bytes) {
        var payload = null;
        try { payload = decodePayload(eventName, bytes); } catch (e) { payload = null; }
        if (payload === null) return;
        var parts = parseEventName(eventName);
        if (!parts) return;
        emitRecord({
            kind: parts.kind,
            dir: dir,
            event: eventName,
            topic: parts.topic,
            field: parts.field,
            payload: payload
        });
    }

    function emitControlRecord(ev) {
        var detail = ev && ev.detail;
        var payload = null;
        if (detail !== undefined && detail !== null) {
            if (typeof detail === "object") {
                payload = {};
                for (var k in detail) {
                    try { payload[k] = typeof detail[k] === "object" && detail[k] !== null
                        ? JSON.parse(JSON.stringify(detail[k])) : detail[k]; }
                    catch (e) { /* non-serializable detail field skipped */ }
                }
            } else {
                payload = detail;
            }
        }
        emitRecord({
            kind: "control",
            dir: "pub",
            event: ev.type,
            topic: payload && typeof payload.key === "string" ? payload.key : null,
            field: null,
            payload: payload
        });
    }

    // ── Trap installation (must run BEFORE gothic-core.js assigns its globals:
    // this inline dev script executes while parsing <body>; gothic-core.js is
    // a deferred <head> script, so its assignments always come later and fall
    // into these setters). The getters stay undefined until the real object is
    // installed, so the runtime's own `if(!window.__gothic_topic)` idempotence
    // guards still see "unset" and go through the assignment path. ──

    var realTopic = null, topicWrapped = null;

    function buildTopicWrapper(orig) {
        var wrapped = Object.create(Object.prototype);
        for (var k in orig) { if (typeof orig[k] === "function") wrapped[k] = orig[k].bind(orig); }
        wrapped.set = function(keyName) {
            orig.set.apply(orig, arguments);
            snapshotFrame(orig, keyName, "pub");
        };
        wrapped.setBytes = function(keyName, u8) {
            orig.setBytes(keyName, u8);
            snapshotFrame(orig, keyName, "bcast");
        };
        return wrapped;
    }

    // snapshotFrame: right after a deposit the pooled per-key view holds
    // exactly that frame — copy it immediately, BEFORE a later deposit for the
    // same key reuses the view, and queue for the event's next dispatch.
    function snapshotFrame(orig, keyName, dir) {
        try {
            if (typeof orig.get !== "function") return;
            var view = orig.get(keyName);
            if (!view || !view.length) return;
            var snap = new Uint8Array(view.length);
            snap.set(view);
            snap._dir = dir;
            (fifo[keyName] || (fifo[keyName] = [])).push(snap);
        } catch (e) { /* observation must never break the page */ }
    }

    try {
        Object.defineProperty(window, "__gothic_topic", {
            configurable: true,
            get: function() { return topicWrapped; },
            set: function(v) {
                if (!v || realTopic === v) return;
                realTopic = v;
                topicWrapped = buildTopicWrapper(v);
            }
        });
    } catch (e) { /* property already non-configurable — observer disabled */ }

    var realDispatch = null;
    function buildDispatchWrapper(orig) {
        return function(name) {
            try {
                var q = fifo[name];
                if (q && q.length) {
                    var frame = q.shift();
                    emitBusRecord(name, frame._dir || "pub", frame);
                }
            } catch (e) { /* observation must never break the page */ }
            return orig.apply(this, arguments);
        };
    }

    try {
        Object.defineProperty(window, "__gothicDispatchAsync", {
            configurable: true,
            get: function() { return dispatchWrapped; },
            set: function(v) {
                // gothic-core.js's own `if(!window.__gothicDispatchAsync)` guard
                // reads our getter: until the real function lands it returns
                // undefined, so the runtime installs its implementation and the
                // assignment falls into this setter. Any assignment IS the real
                // implementation; the wrapper pairs it with the FIFO.
                if (typeof v !== "function" || realDispatchAsync === v) return;
                realDispatchAsync = v;
                dispatchWrapped = buildDispatchWrapper(v);
            }
        });
    } catch (e) { /* property already non-configurable — observer disabled */ }
    var dispatchWrapped = null;
    var realDispatchAsync = null;

    try {
        realDispatch = document.dispatchEvent;
        document.dispatchEvent = function(ev) {
            try {
                if (ev && typeof ev.type === "string" && ev.type.indexOf("gothic:core:") === 0 &&
                    ev.detail !== undefined && ev.detail !== null) {
                    emitControlRecord(ev);
                }
            } catch (e) { /* observation must never break the page */ }
            return realDispatch.apply(document, arguments);
        };
    } catch (e) { /* non-writable dispatchEvent — control-plane capture disabled */ }

    // Export the pure surface for tests / future tooling.
    window.__gothicDevDecode = {
        parseDescriptor: parseDescriptor,
        parseSchemas: parseSchemas,
        parseEventName: parseEventName,
        decodePayload: decodePayload,
        hexDecode: hexDecode,
        hexEncode: hexEncode,
        makeReader: makeReader
    };

    // ── Act marks + settle signals ────────────────────────────────────────────
    //
    // The capture timeline's annotation layer: which element was touched by
    // which input, and when the page went quiet. Marks ride the same batching
    // as bus records (kind:"mark", dir:"act"); each carries the record's
    // epoch-ms t, so a consumer can compute "the error happened 220ms after
    // my click" without page access.
    //
    // Settle is a debounced quiet signal: every mark (and every burst of
    // marks) pushes a timer forward; when the page stays quiet for
    // SETTLE_QUIET_MS the timer fires one "settle" mark. The recorder reads
    // these to end a take on a settled frame; the page itself never waits on
    // them — observation only, never blocking.

    var MARK_EVENTS = ["click", "input", "keydown", "scroll", "submit", "focusin", "change"];
    var SETTLE_QUIET_MS = 250;
    // Burst throttle: keydown repeat / scroll streams can flood the queue;
    // one mark per type per gap keeps the timeline readable.
    var MARK_TYPE_GAP_MS = 50;
    var SCROLL_GAP_MS = 100;

    var settleTimer = null;
    var lastMarkMs = Object.create(null); // event type → epoch ms of last mark

    function markGapFor(type) {
        if (type === "scroll") return SCROLL_GAP_MS;
        if (type === "input" || type === "keydown") return MARK_TYPE_GAP_MS;
        return 0; // click/change/submit/focusin: every occurrence matters
    }

    function armSettle() {
        if (settleTimer) clearTimeout(settleTimer);
        settleTimer = setTimeout(function() {
            settleTimer = null;
            emitRecord({
                kind: "mark",
                dir: "act",
                event: "settle",
                topic: null,
                field: null,
                payload: { quiet_ms: SETTLE_QUIET_MS, scroll_y: window.scrollY }
            });
        }, SETTLE_QUIET_MS);
    }

    // cssPathOf builds a short identifying path for the event target, same
    // style the page manifest uses (id short-circuits, classes capped, tail
    // kept when the path is long).
    function cssPathOf(el) {
        var parts = [];
        var depth = 0;
        while (el && el.nodeType === 1 && depth < 6) {
            var sel = el.nodeName.toLowerCase();
            if (el.id) { parts.unshift(sel + "#" + el.id); break; }
            if (el.classList && el.classList.length) {
                sel += "." + Array.prototype.slice.call(el.classList, 0, 2).join(".");
            }
            parts.unshift(sel);
            el = el.parentElement;
            depth++;
        }
        var out = parts.join(" > ");
        if (out.length > 120) out = out.slice(-120);
        return out || "*";
    }

    function emitMark(ev) {
        var type = ev.type;
        var gap = markGapFor(type);
        var now = Date.now();
        if (gap && lastMarkMs[type] && now - lastMarkMs[type] < gap) {
            armSettle(); // throttled, but still evidence of activity
            return;
        }
        lastMarkMs[type] = now;
        var data = { sel: cssPathOf(ev.target), trusted: ev.isTrusted };
        if (type === "scroll") {
            data.x = window.scrollX; data.y = window.scrollY;
        } else if (ev.clientX !== undefined) {
            data.x = ev.clientX; data.y = ev.clientY;
        }
        if (ev.key !== undefined && ev.key !== "") data.key = ev.key;
        emitRecord({
            kind: "mark",
            dir: "act",
            event: type,
            topic: null,
            field: null,
            payload: data
        });
        armSettle();
    }

    try {
        var onMark = function(ev) {
            try { emitMark(ev); } catch (e) { /* observation must never break the page */ }
        };
        for (var mi = 0; mi < MARK_EVENTS.length; mi++) {
            document.addEventListener(MARK_EVENTS[mi], onMark, { capture: true, passive: true });
        }
        // Boot quiet: a page that loads and settles announces it, so a take
        // that starts on a fresh document still ends on a settled signal.
        armSettle();
    } catch (e) { /* mark capture disabled — bus decoding above is unaffected */ }
})();
