const offlinePage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="theme-color" content="#101415">
  <title>Talker is offline</title>
  <style>
    :root { color-scheme: dark; font-family: "Segoe UI", "Helvetica Neue", Arial, sans-serif; background: #101415; color: #edf2ed; }
    body { margin: 0; min-height: 100svh; display: grid; place-items: center; }
    main { max-width: 28rem; padding: 2rem; }
    h1 { font-size: 2rem; font-weight: 500; letter-spacing: -.05em; }
    p { color: #9ca9aa; line-height: 1.7; }
    a { display: inline-block; margin-top: 1rem; padding: .8rem 1.2rem; border-radius: .4rem; background: #92e4de; color: #101415; font-weight: 600; text-decoration: none; }
    a:focus-visible { outline: 2px solid #92e4de; outline-offset: 5px; }
  </style>
</head>
<body>
  <main>
    <h1>Talker is offline.</h1>
    <p>Talker needs a connection for voice and background tasks. Check your connection to the server, then try again.</p>
    <a href="/">Try again</a>
  </main>
</body>
</html>`;

self.addEventListener("fetch", event => {
  const { request } = event;
  if (request.method !== "GET" || request.mode !== "navigate") return;
  const url = new URL(request.url);
  if (url.origin !== self.location.origin || url.pathname !== "/") return;

  // Only the root document gets a fallback; private data and audio bypass this worker.
  event.respondWith(fetch(request, { cache: "no-store" }).catch(() => new Response(offlinePage, {
    status: 503,
    headers: {
      "Content-Type": "text/html; charset=utf-8",
      "Cache-Control": "no-store",
      "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
      "Referrer-Policy": "no-referrer",
      "X-Content-Type-Options": "nosniff",
    },
  })));
});
