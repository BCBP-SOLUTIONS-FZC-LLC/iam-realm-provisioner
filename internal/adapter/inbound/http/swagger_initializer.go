package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// SwaggerInitializerHandler serves swagger-initializer.js with a plugin that
// keeps model collapse toggles visible after expand.
func SwaggerInitializerHandler(c *gin.Context) {
	c.Header("Content-Type", "application/javascript; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, swaggerInitializerJS)
}

const swaggerInitializerJS = `
window.onload = function() {
  const KeepModelTogglePlugin = function() {
    return {
      wrapComponents: {
        ModelCollapse: function(Original, system) {
          return function(props) {
            var next = Object.assign({}, props);
            if (props.collapsedContent === '[...]') {
              next.hideSelfOnExpand = false;
            }
            return system.React.createElement(Original, next);
          };
        }
      }
    };
  };

  const ui = SwaggerUIBundle({
    url: "doc.json",
    dom_id: '#swagger-ui',
    validatorUrl: null,
    oauth2RedirectUrl: ` + "`${window.location.protocol}//${window.location.host}${window.location.pathname.split('/').slice(0, window.location.pathname.split('/').length - 1).join('/')}/oauth2-redirect.html`" + `,
    persistAuthorization: false,
    presets: [
      SwaggerUIBundle.presets.apis,
      SwaggerUIStandalonePreset
    ],
    plugins: [
      SwaggerUIBundle.plugins.DownloadUrl,
      KeepModelTogglePlugin
    ],
    layout: "StandaloneLayout",
    docExpansion: "list",
    deepLinking: true,
    defaultModelsExpandDepth: 1
  });

  window.ui = ui;
};
`
