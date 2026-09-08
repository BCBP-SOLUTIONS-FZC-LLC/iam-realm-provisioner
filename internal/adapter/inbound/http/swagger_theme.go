package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SwaggerThemeHandler serves the BCBP-branded CSS overrides for Swagger UI.
func SwaggerThemeHandler(c *gin.Context) {
	c.Header("Content-Type", "text/css; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, swaggerThemeCSS)
}

const swaggerThemeCSS = `
@import url('https://fonts.googleapis.com/css2?family=Poppins:wght@300;400;500;600;700&display=swap');

/* ── Base ─────────────────────────────────────────────────────────────────── */
body, .swagger-ui { background: #000 !important; font-family: "Poppins", sans-serif !important; }
.swagger-ui * { box-sizing: border-box; }

/* ── Topbar ───────────────────────────────────────────────────────────────── */
.swagger-ui .topbar {
  background: #000 !important;
  border-bottom: 2px solid;
  border-image: linear-gradient(to right, #8426b0 3%, #bd0283 47%, #ec4b3c 98%) 1;
  padding: .75rem 0;
}
.swagger-ui .topbar a, .swagger-ui .topbar .topbar-wrapper span { color: #fff !important; }
.swagger-ui .topbar .download-url-wrapper input[type=text] {
  background: #0d0d0d !important; border-color: rgba(132,38,176,.4) !important; color: #fff !important;
}
.swagger-ui .topbar .download-url-wrapper .download-url-button {
  background: linear-gradient(to right, #8426b0, #bd0283) !important; border: none !important; color: #fff !important;
}

/* ── Info section ─────────────────────────────────────────────────────────── */
.swagger-ui .information-container {
  background: #0d0d0d !important;
  border-bottom: 2px solid !important;
  border-image: linear-gradient(to right,#8426b0 3%,#bd0283 47%,#ec4b3c 98%) 1 !important;
  padding: 1.5rem 0 !important;
}
.swagger-ui .info .title {
  font-family: "Poppins", sans-serif !important;
  font-weight: 700 !important;
  background: linear-gradient(to right,#8426b0 3%,#bd0283 47%,#ec4b3c 98%) !important;
  -webkit-background-clip: text !important;
  -webkit-text-fill-color: transparent !important;
  background-clip: text !important;
}

/* Base URL strip */
.swagger-ui .info .base-url, .swagger-ui .base-url {
  background: #000 !important;
  border: 1px solid rgba(132,38,176,.4) !important;
  border-radius: 6px !important;
  color: #e879f9 !important;
  -webkit-text-fill-color: #e879f9 !important;
  display: inline-block !important;
  font-family: monospace !important;
  font-size: .85rem !important;
  padding: 3px 10px !important;
  margin: .5rem 0 !important;
}

/* doc.json URL */
.swagger-ui .info .url { color: #9ca3af !important; font-size: .8rem !important; }

/* Contact & license */
.swagger-ui .info__contact, .swagger-ui .info__license { margin-top: .5rem !important; }
.swagger-ui .info__contact a {
  background: linear-gradient(to right,#8426b0,#bd0283);
  -webkit-background-clip: text !important;
  -webkit-text-fill-color: transparent !important;
  background-clip: text !important;
  font-weight: 600 !important;
}
.swagger-ui .info__license a { color: #9ca3af !important; -webkit-text-fill-color: #9ca3af !important; }
.swagger-ui .info p, .swagger-ui .info li, .swagger-ui .info h1, .swagger-ui .info h3 { color: #d1d5db !important; }
.swagger-ui .info a { color: #bd0283 !important; }
.swagger-ui .info code { background: #0d0d0d !important; color: #e879f9 !important; }

/* ── Scheme / auth bar ────────────────────────────────────────────────────── */
.swagger-ui .scheme-container {
  background: #050505 !important; box-shadow: none !important;
  border-bottom: 1px solid rgba(132,38,176,.25) !important;
}
.swagger-ui .schemes > label { color: #9ca3af !important; }

/* ── Operation tags ───────────────────────────────────────────────────────── */
.swagger-ui .opblock-tag {
  color: #fff !important; border-bottom: 1px solid rgba(132,38,176,.25) !important;
  font-family: "Poppins", sans-serif !important; font-weight: 600 !important;
}
.swagger-ui .opblock-tag:hover { background: rgba(132,38,176,.08) !important; }
.swagger-ui .opblock-tag-section h3 { color: #fff !important; }

/* ── Operation blocks (shared) ────────────────────────────────────────────── */
.swagger-ui .opblock {
  background: #0d0d0d !important; border-color: rgba(132,38,176,.3) !important;
  border-radius: 8px !important; margin-bottom: .5rem !important;
}
.swagger-ui .opblock .opblock-summary { border-color: rgba(132,38,176,.3) !important; }
.swagger-ui .opblock .opblock-summary-path,
.swagger-ui .opblock .opblock-summary-path__deprecated { color: #fff !important; font-family: monospace !important; }
.swagger-ui .opblock .opblock-summary-description { color: #9ca3af !important; }
.swagger-ui .opblock-body { background: #0a0a0a !important; }
.swagger-ui .opblock-description-wrapper p,
.swagger-ui .opblock-external-docs-wrapper p,
.swagger-ui .opblock-title_normal p { color: #d1d5db !important; }

/* ── Method colors ────────────────────────────────────────────────────────── */
.swagger-ui .opblock.opblock-get  { border-color: #1e3a5f !important; background: rgba(30,58,95,.08) !important; }
.swagger-ui .opblock.opblock-get  .opblock-summary-method { background: #1e3a5f !important; color: #60a5fa !important; }
.swagger-ui .opblock.opblock-post { border-color: #1a3028 !important; background: rgba(26,48,40,.08) !important; }
.swagger-ui .opblock.opblock-post .opblock-summary-method { background: #1a3028 !important; color: #4ade80 !important; }
.swagger-ui .opblock.opblock-put  { border-color: rgba(132,38,176,.5) !important; background: rgba(132,38,176,.06) !important; }
.swagger-ui .opblock.opblock-put  .opblock-summary-method { background: linear-gradient(to right,#8426b0,#bd0283) !important; color: #fff !important; }
.swagger-ui .opblock.opblock-patch { border-color: rgba(189,2,131,.5) !important; background: rgba(189,2,131,.06) !important; }
.swagger-ui .opblock.opblock-patch .opblock-summary-method { background: linear-gradient(to right,#bd0283,#ec4b3c) !important; color: #fff !important; }
.swagger-ui .opblock.opblock-delete { border-color: #ec4b3c !important; background: rgba(236,75,60,.06) !important; }
.swagger-ui .opblock.opblock-delete .opblock-summary-method { background: #ec4b3c !important; color: #fff !important; }

/* ── Parameters & tables ──────────────────────────────────────────────────── */
.swagger-ui .opblock .opblock-section-header {
  background: rgba(132,38,176,.08) !important;
  border-bottom: 1px solid rgba(132,38,176,.2) !important;
  box-shadow: none !important;
  padding: 12px 16px !important;
}
.swagger-ui .opblock .opblock-section-header h4 {
  color: #fff !important;
  font-family: "Poppins", sans-serif !important;
  font-weight: 600 !important;
}
.swagger-ui .opblock .opblock-section-header .btn.try-out__btn {
  background: transparent !important;
  border: 1px solid rgba(132,38,176,.45) !important;
  color: #e879f9 !important;
}
.swagger-ui .opblock .opblock-section-header .btn.try-out__btn:hover {
  background: rgba(132,38,176,.14) !important;
  border-color: #8426b0 !important;
  color: #fff !important;
}
.swagger-ui .opblock .opblock-section { background: transparent !important; }
.swagger-ui .parameters-container,
.swagger-ui .opblock-section .table-container {
  background: #0a0a0a !important;
  padding: 8px 16px 16px !important;
}
.swagger-ui .parameters-col_description input[type=text],
.swagger-ui .parameters-col_description textarea {
  background: #111 !important;
  border: 1px solid rgba(132,38,176,.35) !important;
  color: #fff !important;
}
.swagger-ui .parameters-container .parameter__name { color: #fff !important; font-weight: 600 !important; }
.swagger-ui .parameters-container .parameter__type { color: #e879f9 !important; font-family: monospace !important; }
.swagger-ui .parameters-container .parameter__in { color: #9ca3af !important; font-style: italic !important; }
.swagger-ui .parameters-container .parameter__deprecated { color: #ec4b3c !important; }
.swagger-ui table thead tr td, .swagger-ui table thead tr th { color: #9ca3af !important; border-bottom: 1px solid rgba(132,38,176,.25) !important; }
.swagger-ui table tbody tr td { color: #d1d5db !important; border-bottom: 1px solid rgba(132,38,176,.12) !important; }

/* ── Inputs ───────────────────────────────────────────────────────────────── */
.swagger-ui input[type=text], .swagger-ui input[type=password],
.swagger-ui input[type=search], .swagger-ui input[type=email],
.swagger-ui textarea, .swagger-ui select {
  background: #0d0d0d !important; border: 1px solid rgba(132,38,176,.4) !important; color: #fff !important;
}
.swagger-ui input:focus, .swagger-ui textarea:focus {
  border-color: #8426b0 !important; outline: none !important;
}

/* ── Buttons ──────────────────────────────────────────────────────────────── */
.swagger-ui .btn { font-family: "Poppins", sans-serif !important; font-weight: 500 !important; border-radius: 6px !important; }
.swagger-ui .btn.authorize {
  background: linear-gradient(to right, #8426b0, #bd0283) !important;
  border-color: transparent !important; color: #fff !important;
}
.swagger-ui .btn.execute {
  background: linear-gradient(to right, #8426b0, #bd0283, #ec4b3c) !important;
  border-color: transparent !important; color: #fff !important;
}
.swagger-ui .btn.cancel { background: #ec4b3c !important; border-color: #ec4b3c !important; color: #fff !important; }
.swagger-ui .btn-clear { color: #ec4b3c !important; border-color: #ec4b3c !important; }

/* ── Responses ────────────────────────────────────────────────────────────── */
.swagger-ui .responses-wrapper { background: #0a0a0a !important; }
.swagger-ui .responses-inner { background: #0a0a0a !important; padding: 0 16px 16px !important; }
.swagger-ui .response-col_status { color: #22c55e !important; font-weight: 700 !important; }

/* ── Code / highlight ─────────────────────────────────────────────────────── */
.swagger-ui .microlight, .swagger-ui .highlight-code { background: #0d0d0d !important; color: #e879f9 !important; }
.swagger-ui pre:not(.version) { background: #0d0d0d !important; color: #e879f9 !important; border: 1px solid rgba(132,38,176,.25) !important; border-radius: 6px !important; }
.swagger-ui code { background: #0d0d0d !important; color: #e879f9 !important; }

/* ── Models section ───────────────────────────────────────────────────────── */
.swagger-ui section.models {
  border: 1px solid rgba(132,38,176,.25) !important; background: #0d0d0d !important; border-radius: 8px !important;
}
.swagger-ui section.models h4 { color: #fff !important; }
.swagger-ui .model-container { background: #111 !important; border-radius: 6px !important; margin: .5rem 0 !important; }
.swagger-ui .model-box { background: #111 !important; }
.swagger-ui .model { color: #d1d5db !important; }
.swagger-ui .model-title { color: #fff !important; font-weight: 600 !important; }
.swagger-ui .prop-format { color: #f59e0b !important; }
.swagger-ui .required { color: #ec4b3c !important; font-weight: 600 !important; }

/* ── Auth modal ───────────────────────────────────────────────────────────── */
.swagger-ui .dialog-ux .modal-ux {
  background: #0d0d0d !important; border: 1px solid rgba(132,38,176,.4) !important; border-radius: 8px !important;
}
.swagger-ui .dialog-ux .modal-ux-header {
  border-bottom: 1px solid rgba(132,38,176,.25) !important; background: #000 !important;
}
.swagger-ui .dialog-ux .modal-ux-header h3 { color: #fff !important; }
.swagger-ui .dialog-ux .modal-ux-content p,
.swagger-ui .dialog-ux .modal-ux-content h4 { color: #d1d5db !important; }

/* ── Misc ─────────────────────────────────────────────────────────────────── */
.swagger-ui p, .swagger-ui h1, .swagger-ui h3,
.swagger-ui h4, .swagger-ui h5, .swagger-ui h6 { color: #fff !important; }
.swagger-ui span, .swagger-ui label { color: #d1d5db !important; }
.swagger-ui a { color: #bd0283 !important; }
.swagger-ui a:hover { color: #ec4b3c !important; }
.swagger-ui svg { fill: #d1d5db !important; }
.swagger-ui .expand-operation svg,
.swagger-ui .expand-methods svg,
.swagger-ui .models-control svg,
.swagger-ui svg.arrow {
  fill: #e879f9 !important;
}

/* ── API version badge ────────────────────────────────────────────────────── */
.swagger-ui .info .title small,
.swagger-ui .info .title small.version-stamp {
  background: linear-gradient(to right, #8426b0, #bd0283) !important;
  -webkit-background-clip: padding-box !important;
  background-clip: padding-box !important;
  -webkit-text-fill-color: initial !important;
  border-radius: 4px !important;
  display: inline-block !important;
  margin: 0 0 0 .5rem !important;
  padding: 2px 8px !important;
  vertical-align: middle !important;
}
.swagger-ui .info pre.version {
  background: none !important;
  border: none !important;
  color: #fff !important;
  -webkit-text-fill-color: #fff !important;
  font-family: "Poppins", sans-serif !important;
  font-size: .75rem !important;
  font-weight: 600 !important;
  margin: 0 !important;
  padding: 0 !important;
}
.swagger-ui .loading-container .loading::after { border-color: #8426b0 transparent transparent !important; }
`
