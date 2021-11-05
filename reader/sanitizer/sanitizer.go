// Copyright 2017 Frédéric Guillot. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package sanitizer // import "miniflux.app/reader/sanitizer"

import (
	"bytes"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"io"
	"regexp"
	"strconv"
	"strings"

	"miniflux.app/url"

	"golang.org/x/net/html"
)

var (
	youtubeEmbedRegex = regexp.MustCompile(`//www\.youtube\.com/embed/(.*)`)
	splitSrcsetRegex  = regexp.MustCompile(`,\s?`)
)

// Sanitize returns safe HTML.
func Sanitize(baseURL, input string) string {
	var buffer bytes.Buffer
	var tagStack []string
	var parentTag string
	blacklistedTagDepth := 0

	tokenizer := html.NewTokenizer(bytes.NewBufferString(input))
	for {
		if tokenizer.Next() == html.ErrorToken {
			err := tokenizer.Err()
			if err == io.EOF {
				return postSanitize(buffer.String())
			}

			return ""
		}

		token := tokenizer.Token()
		if blacklistedTagDepth > 0 && token.Type != html.EndTagToken {
			continue
		}
		switch token.Type {
		case html.TextToken:
			// An iframe element never has fallback content.
			// See https://www.w3.org/TR/2010/WD-html5-20101019/the-iframe-element.html#the-iframe-element
			if parentTag == "iframe" {
				continue
			}

			buffer.WriteString(html.EscapeString(token.Data))
		case html.StartTagToken:
			tagName := token.DataAtom.String()
			parentTag = tagName

			if !isPixelTracker(tagName, token.Attr) && isValidTag(tagName) {
				attrNames, htmlAttributes := sanitizeAttributes(baseURL, tagName, token.Attr)

				if hasRequiredAttributes(tagName, attrNames) {
					if len(attrNames) > 0 {
						buffer.WriteString("<" + tagName + " " + htmlAttributes + ">")
					} else {
						buffer.WriteString("<" + tagName + ">")
					}

					tagStack = append(tagStack, tagName)
				}
			} else if isBlockedTag(token.Data) {
				blacklistedTagDepth++
			}
		case html.EndTagToken:
			tagName := token.DataAtom.String()
			if isValidTag(tagName) && inList(tagName, tagStack) {
				buffer.WriteString(fmt.Sprintf("</%s>", tagName))
			} else if isBlockedTag(token.Data) {
				blacklistedTagDepth--
			}
		case html.SelfClosingTagToken:
			tagName := token.DataAtom.String()
			if !isPixelTracker(tagName, token.Attr) && isValidTag(tagName) {
				attrNames, htmlAttributes := sanitizeAttributes(baseURL, tagName, token.Attr)

				if hasRequiredAttributes(tagName, attrNames) {
					if len(attrNames) > 0 {
						buffer.WriteString("<" + tagName + " " + htmlAttributes + "/>")
					} else {
						buffer.WriteString("<" + tagName + "/>")
					}
				}
			}
		}
	}
}

var brSentenceRegex = regexp.MustCompile("([^>]*)<br/>")
func postSanitize(input string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(input))
	if err != nil {
		return ""
	}
	// remove empty p
	doc.Find("p").FilterFunction(func(i int, s *goquery.Selection) bool {
		if s.Children().Size() > 0 {
			return false
		}
		return strings.TrimSpace(s.Text()) == ""
	}).Remove()
	str, err := doc.Find("body").First().Html()
	if err != nil {
		return ""
	}
	// replace br with p
	str = brSentenceRegex.ReplaceAllString(str, `<p>${1}</p>`)
	return str
}

func sanitizeAttributes(baseURL, tagName string, attributes []html.Attribute) ([]string, string) {
	var htmlAttrs, attrNames []string
	var err error

	for _, attribute := range attributes {
		value := attribute.Val

		if !isValidAttribute(tagName, attribute.Key) {
			continue
		}

		if (tagName == "img" || tagName == "source") && attribute.Key == "srcset" {
			value = sanitizeSrcsetAttr(baseURL, value)
		}

		if isExternalResourceAttribute(attribute.Key) {
			if tagName == "iframe" {
				if isValidIframeSource(baseURL, attribute.Val) {
					value = rewriteIframeURL(attribute.Val)
				} else {
					continue
				}
			} else if tagName == "img" && attribute.Key == "src" && isValidDataAttribute(attribute.Val) {
				value = attribute.Val
			} else {
				value, err = url.AbsoluteURL(baseURL, value)
				if err != nil {
					continue
				}

				if !hasValidURIScheme(value) || isBlockedResource(value) {
					continue
				}
			}
		}

		attrNames = append(attrNames, attribute.Key)
		htmlAttrs = append(htmlAttrs, fmt.Sprintf(`%s="%s"`, attribute.Key, html.EscapeString(value)))
	}

	extraAttrNames, extraHTMLAttributes := getExtraAttributes(tagName)
	if len(extraAttrNames) > 0 {
		attrNames = append(attrNames, extraAttrNames...)
		htmlAttrs = append(htmlAttrs, extraHTMLAttributes...)
	}

	return attrNames, strings.Join(htmlAttrs, " ")
}

func getExtraAttributes(tagName string) ([]string, []string) {
	switch tagName {
	case "a":
		return []string{"rel", "target", "referrerpolicy"}, []string{`rel="noopener noreferrer"`, `target="_blank"`, `referrerpolicy="no-referrer"`}
	case "video", "audio":
		return []string{"controls"}, []string{"controls"}
	case "iframe":
		return []string{"sandbox", "loading"}, []string{`sandbox="allow-scripts allow-same-origin allow-popups"`, `loading="lazy"`}
	case "img":
		return []string{"loading"}, []string{`loading="lazy"`}
	default:
		return nil, nil
	}
}

func isValidTag(tagName string) bool {
	for element := range getTagAllowList() {
		if tagName == element {
			return true
		}
	}

	return false
}

func isValidAttribute(tagName, attributeName string) bool {
	for element, attributes := range getTagAllowList() {
		if tagName == element {
			if inList(attributeName, attributes) {
				return true
			}
		}
	}

	return false
}

func isExternalResourceAttribute(attribute string) bool {
	switch attribute {
	case "src", "href", "poster", "cite":
		return true
	default:
		return false
	}
}

func isPixelTracker(tagName string, attributes []html.Attribute) bool {
	if tagName == "img" {
		hasHeight := false
		hasWidth := false

		for _, attribute := range attributes {
			if attribute.Key == "height" && attribute.Val == "1" {
				hasHeight = true
			}

			if attribute.Key == "width" && attribute.Val == "1" {
				hasWidth = true
			}
		}

		return hasHeight && hasWidth
	}

	return false
}

func hasRequiredAttributes(tagName string, attributes []string) bool {
	elements := make(map[string][]string)
	elements["a"] = []string{"href"}
	elements["iframe"] = []string{"src"}
	elements["img"] = []string{"src"}
	elements["source"] = []string{"src", "srcset"}

	for element, attrs := range elements {
		if tagName == element {
			for _, attribute := range attributes {
				for _, attr := range attrs {
					if attr == attribute {
						return true
					}
				}
			}

			return false
		}
	}

	return true
}

// See https://www.iana.org/assignments/uri-schemes/uri-schemes.xhtml
func hasValidURIScheme(src string) bool {
	whitelist := []string{
		"apt:",
		"bitcoin:",
		"callto:",
		"dav:",
		"davs:",
		"ed2k://",
		"facetime://",
		"feed:",
		"ftp://",
		"geo:",
		"gopher://",
		"git://",
		"http://",
		"https://",
		"irc://",
		"irc6://",
		"ircs://",
		"itms://",
		"itms-apps://",
		"magnet:",
		"mailto:",
		"news:",
		"nntp:",
		"rtmp://",
		"sip:",
		"sips:",
		"skype:",
		"spotify:",
		"ssh://",
		"sftp://",
		"steam://",
		"svn://",
		"svn+ssh://",
		"tel:",
		"webcal://",
		"xmpp:",
	}

	for _, prefix := range whitelist {
		if strings.HasPrefix(src, prefix) {
			return true
		}
	}

	return false
}

func isBlockedResource(src string) bool {
	blacklist := []string{
		"feedsportal.com",
		"api.flattr.com",
		"stats.wordpress.com",
		"plus.google.com/share",
		"twitter.com/share",
		"feeds.feedburner.com",
	}

	for _, element := range blacklist {
		if strings.Contains(src, element) {
			return true
		}
	}

	return false
}

func isValidIframeSource(baseURL, src string) bool {
	whitelist := []string{
		"https://invidio.us",
		"//www.youtube.com",
		"http://www.youtube.com",
		"https://www.youtube.com",
		"https://www.youtube-nocookie.com",
		"http://player.vimeo.com",
		"https://player.vimeo.com",
		"http://www.dailymotion.com",
		"https://www.dailymotion.com",
		"http://vk.com",
		"https://vk.com",
		"http://soundcloud.com",
		"https://soundcloud.com",
		"http://w.soundcloud.com",
		"https://w.soundcloud.com",
		"http://bandcamp.com",
		"https://bandcamp.com",
		"https://cdn.embedly.com",
		"https://player.bilibili.com",
		"http://player.bilibili.com",
		"https://store.steampowered.com/widget/",
		"//player.bilibili.com",
		"https://v.qq.com/txp/iframe/player.html",
		"//music.163.com/outchain/player",
		"https://video.h5.weibo.cn",
		"https://h5.video.weibo.com",
		"https://v.miaopai.com/iframe",
	}

	// allow iframe from same origin
	if url.Domain(baseURL) == url.Domain(src) {
		return true
	}

	for _, prefix := range whitelist {
		if strings.HasPrefix(src, prefix) {
			return true
		}
	}

	return false
}

func getTagAllowList() map[string][]string {
	whitelist := make(map[string][]string)
	whitelist["img"] = []string{"alt", "title", "src", "srcset", "sizes"}
	whitelist["picture"] = []string{}
	whitelist["audio"] = []string{"src"}
	whitelist["video"] = []string{"poster", "height", "width", "src"}
	whitelist["source"] = []string{"src", "type", "srcset", "sizes", "media"}
	whitelist["dt"] = []string{}
	whitelist["dd"] = []string{}
	whitelist["dl"] = []string{}
	whitelist["table"] = []string{}
	whitelist["caption"] = []string{}
	whitelist["thead"] = []string{}
	whitelist["tfooter"] = []string{}
	whitelist["tr"] = []string{}
	whitelist["td"] = []string{"rowspan", "colspan"}
	whitelist["th"] = []string{"rowspan", "colspan"}
	whitelist["h1"] = []string{}
	whitelist["h2"] = []string{}
	whitelist["h3"] = []string{}
	whitelist["h4"] = []string{}
	whitelist["h5"] = []string{}
	whitelist["h6"] = []string{}
	whitelist["strong"] = []string{}
	whitelist["em"] = []string{}
	whitelist["code"] = []string{}
	whitelist["pre"] = []string{}
	whitelist["blockquote"] = []string{}
	whitelist["q"] = []string{"cite"}
	whitelist["p"] = []string{}
	whitelist["ul"] = []string{}
	whitelist["li"] = []string{}
	whitelist["ol"] = []string{}
	whitelist["br"] = []string{}
	whitelist["del"] = []string{}
	whitelist["a"] = []string{"href", "title"}
	whitelist["figure"] = []string{}
	whitelist["figcaption"] = []string{}
	whitelist["cite"] = []string{}
	whitelist["time"] = []string{"datetime"}
	whitelist["abbr"] = []string{"title"}
	whitelist["acronym"] = []string{"title"}
	whitelist["wbr"] = []string{}
	whitelist["dfn"] = []string{}
	whitelist["sub"] = []string{}
	whitelist["sup"] = []string{}
	whitelist["var"] = []string{}
	whitelist["samp"] = []string{}
	whitelist["s"] = []string{}
	whitelist["del"] = []string{}
	whitelist["ins"] = []string{}
	whitelist["kbd"] = []string{}
	whitelist["rp"] = []string{}
	whitelist["rt"] = []string{}
	whitelist["rtc"] = []string{}
	whitelist["ruby"] = []string{}
	whitelist["b"] = []string{}
	whitelist["small"] = []string{}
	whitelist["iframe"] = []string{"width", "height", "frameborder", "src", "allowfullscreen", "scrolling"}
	return whitelist
}

func inList(needle string, haystack []string) bool {
	for _, element := range haystack {
		if element == needle {
			return true
		}
	}

	return false
}

func rewriteIframeURL(link string) string {
	matches := youtubeEmbedRegex.FindStringSubmatch(link)
	if len(matches) == 2 {
		return `https://www.youtube-nocookie.com/embed/` + matches[1]
	}

	return link
}

func isBlockedTag(tagName string) bool {
	blacklist := []string{
		"noscript",
		"script",
		"style",
	}

	for _, element := range blacklist {
		if element == tagName {
			return true
		}
	}

	return false
}

/*

One or more strings separated by commas, indicating possible image sources for the user agent to use.

Each string is composed of:
- A URL to an image
- Optionally, whitespace followed by one of:
- A width descriptor (a positive integer directly followed by w). The width descriptor is divided by the source size given in the sizes attribute to calculate the effective pixel density.
- A pixel density descriptor (a positive floating point number directly followed by x).

*/
func sanitizeSrcsetAttr(baseURL, value string) string {
	var sanitizedSources []string
	rawSources := splitSrcsetRegex.Split(value, -1)
	for _, rawSource := range rawSources {
		parts := strings.Split(strings.TrimSpace(rawSource), " ")
		nbParts := len(parts)

		if nbParts > 0 {
			sanitizedSource, err := url.AbsoluteURL(baseURL, parts[0])
			if err != nil {
				continue
			}

			if nbParts == 2 && isValidWidthOrDensityDescriptor(parts[1]) {
				sanitizedSource += " " + parts[1]
			}

			sanitizedSources = append(sanitizedSources, sanitizedSource)
		}
	}
	return strings.Join(sanitizedSources, ", ")
}

func isValidWidthOrDensityDescriptor(value string) bool {
	if value == "" {
		return false
	}

	lastChar := value[len(value)-1:]
	if lastChar != "w" && lastChar != "x" {
		return false
	}

	_, err := strconv.ParseFloat(value[0:len(value)-1], 32)
	return err == nil
}

func isValidDataAttribute(value string) bool {
	var dataAttributeAllowList = []string{
		"data:image/avif",
		"data:image/apng",
		"data:image/png",
		"data:image/svg",
		"data:image/svg+xml",
		"data:image/jpg",
		"data:image/jpeg",
		"data:image/gif",
		"data:image/webp",
	}

	for _, prefix := range dataAttributeAllowList {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
