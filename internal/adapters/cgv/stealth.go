package cgv

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mxschmitt/playwright-go"
)

// stealthBootstrap is the base puppeteer-extra-plugin-stealth evasion bundle
// vendored from jonfriesen/playwright-go-stealth. Keeping the JavaScript local
// avoids pulling its incompatible playwright-community Go types into this
// mxschmitt/playwright-go codebase.
//
//go:embed third_party/playwright_go_stealth/stealth.min.js
var stealthBootstrap string

// chromeStealthBootstrap is upstream's additional Chrome-specific patch. It is
// mandatory for every Cineko browser context and runs immediately after the
// base stealth bundle.
//
//go:embed third_party/playwright_go_stealth/chrome_stealth.js
var chromeStealthBootstrap string

const stealthLanguageArgs = "_args:[{opts:{languages:[]}}]}"
const stealthWebGLArgs = "_args:[{}]}),(()=>{try{if(window.outerWidth&&window.outerHeight)"

type webGLIdentity struct {
	Vendor   string `json:"vendor"`
	Renderer string `json:"renderer"`
}

func stealthBootstrapForIdentity(languages []string, webGL webGLIdentity) (string, error) {
	if len(languages) == 0 {
		return "", errors.New("browser languages are required")
	}
	if strings.TrimSpace(webGL.Vendor) == "" || strings.TrimSpace(webGL.Renderer) == "" {
		return "", errors.New("browser WebGL identity is required")
	}
	if strings.Count(stealthBootstrap, stealthLanguageArgs) != 1 {
		return "", errors.New("vendored stealth language hook changed")
	}
	if strings.Count(stealthBootstrap, stealthWebGLArgs) != 1 {
		return "", errors.New("vendored stealth WebGL hook changed")
	}
	encoded, err := json.Marshal(languages)
	if err != nil {
		return "", err
	}
	replacement := "_args:[{opts:{languages:" + string(encoded) + "}}]}"
	configured := strings.Replace(stealthBootstrap, stealthLanguageArgs, replacement, 1)
	encoded, err = json.Marshal(webGL)
	if err != nil {
		return "", err
	}
	replacement = "_args:[" + string(encoded) + "]}),(()=>{try{if(window.outerWidth&&window.outerHeight)"
	return strings.Replace(configured, stealthWebGLArgs, replacement, 1), nil
}

func readNativeWebGLIdentity(page playwright.Page) (webGLIdentity, error) {
	value, err := page.Evaluate(`(() => {
		const canvas = document.createElement('canvas');
		const context = canvas.getContext('webgl');
		if (!context) return null;
		context.getExtension('WEBGL_debug_renderer_info');
		return {
			vendor: context.getParameter(37445),
			renderer: context.getParameter(37446)
		};
	})()`)
	if err != nil {
		return webGLIdentity{}, fmt.Errorf("read browser WebGL identity: %w", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return webGLIdentity{}, fmt.Errorf("encode browser WebGL identity: %w", err)
	}
	var identity webGLIdentity
	if err := json.Unmarshal(encoded, &identity); err != nil {
		return webGLIdentity{}, fmt.Errorf("decode browser WebGL identity: %w", err)
	}
	if strings.TrimSpace(identity.Vendor) == "" || strings.TrimSpace(identity.Renderer) == "" {
		return webGLIdentity{}, errors.New("browser returned no WebGL identity")
	}
	return identity, nil
}

// The upstream webdriver evasion intentionally leaves Chrome's false value
// alone. Cineko's browser contract requires the property to be absent, so this
// runs after the vendored bundle and removes it from Navigator.prototype.
const cinekoStealthOverrides = `(() => {
	try {
		delete Object.getPrototypeOf(navigator).webdriver;
	} catch (_) {}
})();`
