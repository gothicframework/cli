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
