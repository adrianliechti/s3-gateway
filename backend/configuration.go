package backend

import "encoding/xml"

type UnknownElement struct{ XMLName xml.Name }
type CORSConfiguration struct {
	XMLName xml.Name   `xml:"CORSConfiguration" json:"-"`
	XMLNS   string     `xml:"xmlns,attr,omitempty" json:"-"`
	Rules   []CORSRule `xml:"CORSRule"`
}
type CORSRule struct {
	ID      string   `xml:"ID,omitempty" json:",omitempty"`
	Origins []string `xml:"AllowedOrigin"`
	Methods []string `xml:"AllowedMethod"`
	Headers []string `xml:"AllowedHeader" json:",omitempty"`
	Expose  []string `xml:"ExposeHeader" json:",omitempty"`
	MaxAge  *int     `xml:"MaxAgeSeconds,omitempty" json:",omitempty"`
}
type PublicAccessBlock struct {
	XMLName               xml.Name `xml:"PublicAccessBlockConfiguration" json:"-"`
	XMLNS                 string   `xml:"xmlns,attr,omitempty" json:"-"`
	BlockPublicAcls       bool
	IgnorePublicAcls      bool
	BlockPublicPolicy     bool
	RestrictPublicBuckets bool
}
type LifecycleConfiguration struct {
	XMLName xml.Name         `xml:"LifecycleConfiguration" json:"-"`
	XMLNS   string           `xml:"xmlns,attr,omitempty" json:"-"`
	Rules   []LifecycleRule  `xml:"Rule"`
	Unknown []UnknownElement `xml:",any" json:"-"`
}
type LifecycleRule struct {
	ID                    string `xml:"ID,omitempty"`
	Status                string
	Prefix                *string               `xml:"Prefix,omitempty" json:",omitempty"`
	Filter                *LifecycleFilter      `xml:"Filter,omitempty" json:",omitempty"`
	Expiration            *LifecycleExpiration  `xml:"Expiration,omitempty" json:",omitempty"`
	NoncurrentExpiration  *NoncurrentExpiration `xml:"NoncurrentVersionExpiration,omitempty" json:",omitempty"`
	AbortMultipart        *AbortMultipart       `xml:"AbortIncompleteMultipartUpload,omitempty" json:",omitempty"`
	Transitions           []LifecycleTransition `xml:"Transition,omitempty" json:",omitempty"`
	NoncurrentTransitions []UnknownElement      `xml:"NoncurrentVersionTransition,omitempty" json:"-"`
	Unknown               []UnknownElement      `xml:",any" json:"-"`
}
type LifecycleFilter struct {
	Prefix      *string          `xml:"Prefix,omitempty" json:",omitempty"`
	Tag         *Tag             `xml:"Tag,omitempty" json:",omitempty"`
	And         *LifecycleAnd    `xml:"And,omitempty" json:",omitempty"`
	GreaterThan *int64           `xml:"ObjectSizeGreaterThan,omitempty" json:",omitempty"`
	LessThan    *int64           `xml:"ObjectSizeLessThan,omitempty" json:",omitempty"`
	Unknown     []UnknownElement `xml:",any" json:"-"`
}
type LifecycleAnd struct {
	Prefix      *string          `xml:"Prefix,omitempty" json:",omitempty"`
	Tags        []Tag            `xml:"Tag" json:",omitempty"`
	GreaterThan *int64           `xml:"ObjectSizeGreaterThan,omitempty" json:",omitempty"`
	LessThan    *int64           `xml:"ObjectSizeLessThan,omitempty" json:",omitempty"`
	Unknown     []UnknownElement `xml:",any" json:"-"`
}
type LifecycleExpiration struct {
	Days         *int             `xml:"Days,omitempty" json:",omitempty"`
	Date         string           `xml:"Date,omitempty" json:",omitempty"`
	DeleteMarker *bool            `xml:"ExpiredObjectDeleteMarker,omitempty" json:",omitempty"`
	Unknown      []UnknownElement `xml:",any" json:"-"`
}
type NoncurrentExpiration struct {
	Days          int              `xml:"NoncurrentDays"`
	NewerVersions *int             `xml:"NewerNoncurrentVersions,omitempty" json:",omitempty"`
	Unknown       []UnknownElement `xml:",any" json:"-"`
}
type AbortMultipart struct {
	Days    int              `xml:"DaysAfterInitiation"`
	Unknown []UnknownElement `xml:",any" json:"-"`
}
type LifecycleTransition struct {
	Days         *int   `xml:"Days,omitempty"`
	Date         string `xml:"Date,omitempty"`
	StorageClass string
}
