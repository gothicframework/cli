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
    // The state is kept on window because the paint reload replaces the whole
    // document while the compile is still running; without it the page looks
    // finished for the two or three seconds it is still on the old binary.
    src.addEventListener("building", function(ev) {
        delete window.__gothic_buildError;
        window.__gothic_building = ev.data || "Compiling...";
        showBadge("building", window.__gothic_building);
    });

    // builddone, the WASM stage settled with no error. Sent whether or not any
    // unit was rebuilt, and always before the reload that carries a fresh
    // binary, so the swapped-in document starts clean.
    src.addEventListener("builddone", function() {
        delete window.__gothic_building;
        delete window.__gothic_buildError;
        hideBadge();
    });

    // builderror, compilation failed (payload: error message)
    src.addEventListener("builderror", function(ev) {
        let msg = ev.data || "Unknown build error";
        delete window.__gothic_building;
        window.__gothic_buildError = msg;
        showBadge("failed", msg);
    });

    // message, "reload" (fresh binary ready, swap the page)
    //           or keepalive "ping" (filtered by data guard below)
    src.addEventListener("message", async function(ev) {
        if (!ev || ev.data !== "reload") return;

        // Close SSE before navigating so the server-side slot is freed immediately.
        src.close();
        window.gothicframework_reloadSrc = null;

        try {
            const res = await fetch(window.location.href, { cache: "no-store" });
            if (!res.ok) throw new Error("HTTP " + res.status);
            const html = await res.text();

            // Clear WASM globals so new modules start from a clean registry.
            delete window.__gothic_registry;
            delete window.__gothic_proxied;
            delete window.__gothicCurrentModule;

            // Replace the entire document with the freshly fetched HTML.
            // document.open/write/close forces the browser to re-request all
            // linked resources (CSS, JS) as if it were a brand-new page load.
            document.open("text/html", "replace");
            document.write(html);
            document.close();
        } catch(e) {
            // Fallback if fetch fails or returns a non-OK status (server still restarting).
            window.location.reload();
        }
    });

    // Re-establish the badge after document.write. The reload handler destroys
    // the entire DOM, but the window object survives the swap, so the pending
    // state is still here. An error takes precedence over a running build.
    // This is the case that matters most: the paint reload lands while the
    // compile is still going, and without this the page looks done while it is
    // still running the previous binary.
    if (window.__gothic_buildError) {
        showBadge("failed", window.__gothic_buildError);
    } else if (window.__gothic_building) {
        showBadge("building", window.__gothic_building);
    }

    window.gothicframework_reloadSrc = src;
    window.onbeforeunload = function() { src.close(); };
})();
