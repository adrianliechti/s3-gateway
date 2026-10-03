package gateway

import (
	"encoding/xml"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/adrianliechti/s3-gateway/internal/identity"
)

var sessionName = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)

const stsXMLNS = "https://sts.amazonaws.com/doc/2011-06-15/"

func (g *Gateway) sts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	fail := func(code, message string, status int) {
		writeXML(w, status, struct {
			XMLName   xml.Name `xml:"ErrorResponse"`
			XMLNS     string   `xml:"xmlns,attr"`
			Type      string   `xml:"Error>Type"`
			Code      string   `xml:"Error>Code"`
			Message   string   `xml:"Error>Message"`
			RequestID string   `xml:"RequestId"`
		}{XMLNS: stsXMLNS, Type: "Sender", Code: code, Message: message, RequestID: w.Header().Get("x-amz-request-id")})
	}
	if g.identity == nil {
		fail("InvalidAction", "Web identity is not configured", 400)
		return
	}
	if r.Method != "POST" && r.Method != "GET" {
		fail("InvalidAction", "Unsupported method", 400)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32768)
	if r.ParseForm() != nil {
		fail("InvalidParameterValue", "Invalid STS request", 400)
		return
	}
	for key, values := range r.Form {
		if len(values) != 1 {
			fail("InvalidParameterValue", "Duplicate parameter", 400)
			return
		}
		switch key {
		case "Action", "Version", "RoleArn", "RoleSessionName", "WebIdentityToken", "DurationSeconds":
		default:
			fail("InvalidParameterValue", "Unsupported STS parameter", 400)
			return
		}
	}
	if r.Form.Get("Action") != "AssumeRoleWithWebIdentity" || r.Form.Get("Version") != "2011-06-15" {
		fail("InvalidAction", "Only AssumeRoleWithWebIdentity is supported", 400)
		return
	}
	name := r.Form.Get("RoleSessionName")
	if !sessionName.MatchString(name) {
		fail("InvalidParameterValue", "Invalid role session name", 400)
		return
	}
	duration := 0
	if value := r.Form.Get("DurationSeconds"); value != "" {
		var err error
		duration, err = strconv.Atoi(value)
		if err != nil || duration == 0 {
			fail("InvalidParameterValue", "Invalid duration", 400)
			return
		}
	}
	credentials, who, err := g.identity.Assume(r.Context(), r.Form.Get("WebIdentityToken"), r.Form.Get("RoleArn"), duration)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrRole):
			fail("AccessDenied", "Role assumption denied", 403)
		case errors.Is(err, identity.ErrDuration):
			fail("InvalidParameterValue", "Invalid session duration", 400)
		default:
			fail("InvalidIdentityToken", "Identity token could not be verified", 400)
		}
		return
	}
	arn := strings.Replace(who.Role, ":iam:", ":sts:", 1)
	arn = strings.Replace(arn, ":role/", ":assumed-role/", 1) + "/" + name
	writeXML(w, 200, struct {
		XMLName     xml.Name             `xml:"AssumeRoleWithWebIdentityResponse"`
		XMLNS       string               `xml:"xmlns,attr"`
		Credentials identity.Credentials `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
		Subject     string               `xml:"AssumeRoleWithWebIdentityResult>SubjectFromWebIdentityToken"`
		Audience    string               `xml:"AssumeRoleWithWebIdentityResult>Audience"`
		Provider    string               `xml:"AssumeRoleWithWebIdentityResult>Provider"`
		Arn         string               `xml:"AssumeRoleWithWebIdentityResult>AssumedRoleUser>Arn"`
		ID          string               `xml:"AssumeRoleWithWebIdentityResult>AssumedRoleUser>AssumedRoleId"`
		RequestID   string               `xml:"ResponseMetadata>RequestId"`
	}{XMLNS: stsXMLNS, Credentials: credentials, Subject: who.Subject, Audience: g.identity.Audience(), Provider: g.identity.Issuer(), Arn: arn, ID: sha256Hex(who.Role)[:20] + ":" + name, RequestID: w.Header().Get("x-amz-request-id")})
}
