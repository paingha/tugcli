package apiinfo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
)

// CRDResource is where the API serves custom resource definitions.
var CRDResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// ErrNoCRD is returned when no custom resource definition has a name.
var ErrNoCRD = errors.New("no such custom resource definition")

// CRD is a custom resource definition, as much of it as tug explains.
type CRD struct {
	// Name is plural.group, such as certificates.cert-manager.io.
	Name       string   `json:"name"`
	Group      string   `json:"group"`
	Kind       string   `json:"kind"`
	ListKind   string   `json:"listKind,omitempty"`
	Plural     string   `json:"plural"`
	Singular   string   `json:"singular,omitempty"`
	ShortNames []string `json:"shortNames,omitempty"`
	Categories []string `json:"categories,omitempty"`
	// Scope is Namespaced or Cluster.
	Scope    string       `json:"scope"`
	Versions []CRDVersion `json:"versions"`
	// StoredVersions are the versions objects may be stored at in etcd.
	// A version other than the storage version means some objects may
	// not have been migrated to it yet.
	StoredVersions []string    `json:"storedVersions"`
	Conversion     Conversion  `json:"conversion"`
	Conditions     []Condition `json:"conditions,omitempty"`
}

// CRDVersion is one version a custom resource definition declares.
type CRDVersion struct {
	Name               string          `json:"name"`
	Served             bool            `json:"served"`
	Storage            bool            `json:"storage"`
	Deprecated         bool            `json:"deprecated,omitempty"`
	DeprecationWarning string          `json:"deprecationWarning,omitempty"`
	PrinterColumns     []PrinterColumn `json:"printerColumns,omitempty"`
	// Subresources are status and scale, when the version has them.
	Subresources []string `json:"subresources,omitempty"`
	// Schema is the version's openAPIV3Schema; `tug crd schema` shows it.
	Schema *Schema `json:"-"`
}

// PrinterColumn is a column kubectl get shows for a custom resource.
type PrinterColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`
	JSONPath    string `json:"jsonPath"`
	// Priority above 0 means kubectl shows the column only with -o wide.
	Priority int32 `json:"priority,omitempty"`
}

// Conversion says how the API server converts objects between versions.
type Conversion struct {
	// Strategy is None, where versions differ only in apiVersion, or
	// Webhook.
	Strategy string `json:"strategy"`
	// Webhook is where a conversion webhook runs: a Service as
	// namespace/name:port/path, or a URL.
	Webhook string `json:"webhook,omitempty"`
	// ReviewVersions are the ConversionReview versions the webhook
	// accepts.
	ReviewVersions []string `json:"reviewVersions,omitempty"`
}

// Condition is one of a definition's status conditions, such as
// Established.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Version returns the definition's version with a name.
func (c *CRD) Version(name string) (*CRDVersion, bool) {
	for i := range c.Versions {
		if c.Versions[i].Name == name {
			return &c.Versions[i], true
		}
	}
	return nil, false
}

// Served returns the versions the API serves, most preferred first, as
// discovery orders them.
func (c *CRD) Served() []string {
	var out []string
	for _, v := range c.Versions {
		if v.Served {
			out = append(out, v.Name)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return -version.CompareKubeAwareVersionStrings(a, b) })
	return out
}

// Preferred returns the version clients use by default: the most preferred
// served version, or "" when none is served.
func (c *CRD) Preferred() string {
	if served := c.Served(); len(served) > 0 {
		return served[0]
	}
	return ""
}

// Storage returns the version new objects are stored at.
func (c *CRD) Storage() string {
	for _, v := range c.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

// Unmigrated returns the stored versions other than the storage version:
// objects may still be stored at them, so they cannot be dropped from the
// definition until those objects are rewritten.
func (c *CRD) Unmigrated() []string {
	storage := c.Storage()
	var out []string
	for _, v := range c.StoredVersions {
		if v != storage {
			out = append(out, v)
		}
	}
	return out
}

// ReadCRD reads one custom resource definition by name, wrapping ErrNoCRD
// when there is none.
func ReadCRD(ctx context.Context, dyn dynamic.Interface, name string) (*CRD, error) {
	u, err := dyn.Resource(CRDResource).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%w: %s", ErrNoCRD, name)
	}
	if err != nil {
		return nil, fmt.Errorf("reading custom resource definition %s: %w", name, err)
	}
	return ParseCRD(u)
}

// ListCRDs reads every custom resource definition, sorted by name.
func ListCRDs(ctx context.Context, dyn dynamic.Interface) ([]CRD, error) {
	var out []CRD
	opts := metav1.ListOptions{Limit: 100}
	for {
		list, err := dyn.Resource(CRDResource).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing custom resource definitions: %w", err)
		}
		for i := range list.Items {
			c, err := ParseCRD(&list.Items[i])
			if err != nil {
				return nil, err
			}
			out = append(out, *c)
		}
		if list.GetContinue() == "" {
			break
		}
		opts.Continue = list.GetContinue()
	}
	slices.SortFunc(out, func(a, b CRD) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// CRDNames lists the names of every custom resource definition, reading
// only their metadata, which is far smaller than their schemas.
func CRDNames(ctx context.Context, md metadata.Interface) ([]string, error) {
	var out []string
	opts := metav1.ListOptions{Limit: 500}
	for {
		list, err := md.Resource(CRDResource).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing custom resource definitions: %w", err)
		}
		for _, it := range list.Items {
			out = append(out, it.Name)
		}
		if list.Continue == "" {
			break
		}
		opts.Continue = list.Continue
	}
	slices.Sort(out)
	return out, nil
}

// crdObject is the part of an apiextensions.k8s.io/v1
// CustomResourceDefinition that tug reads.
type crdObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Kind       string   `json:"kind"`
			ListKind   string   `json:"listKind"`
			Plural     string   `json:"plural"`
			Singular   string   `json:"singular"`
			ShortNames []string `json:"shortNames"`
			Categories []string `json:"categories"`
		} `json:"names"`
		Scope    string `json:"scope"`
		Versions []struct {
			Name               string          `json:"name"`
			Served             bool            `json:"served"`
			Storage            bool            `json:"storage"`
			Deprecated         bool            `json:"deprecated"`
			DeprecationWarning *string         `json:"deprecationWarning"`
			PrinterColumns     []PrinterColumn `json:"additionalPrinterColumns"`
			Subresources       map[string]any  `json:"subresources"`
			Schema             *struct {
				OpenAPIV3Schema *Schema `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
		Conversion *struct {
			Strategy string `json:"strategy"`
			Webhook  *struct {
				ClientConfig *struct {
					URL     *string `json:"url"`
					Service *struct {
						Namespace string  `json:"namespace"`
						Name      string  `json:"name"`
						Path      *string `json:"path"`
						Port      *int32  `json:"port"`
					} `json:"service"`
				} `json:"clientConfig"`
				ConversionReviewVersions []string `json:"conversionReviewVersions"`
			} `json:"webhook"`
		} `json:"conversion"`
	} `json:"spec"`
	Status struct {
		StoredVersions []string    `json:"storedVersions"`
		Conditions     []Condition `json:"conditions"`
	} `json:"status"`
}

// ParseCRD reads a custom resource definition the dynamic client returned.
func ParseCRD(u *unstructured.Unstructured) (*CRD, error) {
	b, err := json.Marshal(u.Object)
	if err != nil {
		return nil, fmt.Errorf("reading custom resource definition %s: %w", u.GetName(), err)
	}
	return parseCRDJSON(b)
}

func parseCRDJSON(b []byte) (*CRD, error) {
	var o crdObject
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("reading a custom resource definition: %w", err)
	}
	s := o.Spec
	c := &CRD{
		Name:           o.Metadata.Name,
		Group:          s.Group,
		Kind:           s.Names.Kind,
		ListKind:       s.Names.ListKind,
		Plural:         s.Names.Plural,
		Singular:       s.Names.Singular,
		ShortNames:     s.Names.ShortNames,
		Categories:     s.Names.Categories,
		Scope:          s.Scope,
		StoredVersions: o.Status.StoredVersions,
		Conditions:     o.Status.Conditions,
		Conversion:     Conversion{Strategy: "None"},
	}
	for _, v := range s.Versions {
		cv := CRDVersion{
			Name:           v.Name,
			Served:         v.Served,
			Storage:        v.Storage,
			Deprecated:     v.Deprecated,
			PrinterColumns: v.PrinterColumns,
		}
		if v.DeprecationWarning != nil {
			cv.DeprecationWarning = *v.DeprecationWarning
		}
		for _, sub := range []string{"status", "scale"} {
			if _, ok := v.Subresources[sub]; ok {
				cv.Subresources = append(cv.Subresources, sub)
			}
		}
		if v.Schema != nil {
			cv.Schema = v.Schema.OpenAPIV3Schema
		}
		c.Versions = append(c.Versions, cv)
	}
	if conv := s.Conversion; conv != nil {
		if conv.Strategy != "" {
			c.Conversion.Strategy = conv.Strategy
		}
		if wh := conv.Webhook; wh != nil {
			c.Conversion.ReviewVersions = wh.ConversionReviewVersions
			if cc := wh.ClientConfig; cc != nil {
				switch {
				case cc.URL != nil:
					c.Conversion.Webhook = *cc.URL
				case cc.Service != nil:
					svc := cc.Service
					port := int32(443)
					if svc.Port != nil {
						port = *svc.Port
					}
					c.Conversion.Webhook = svc.Namespace + "/" + svc.Name + ":" + strconv.Itoa(int(port))
					if svc.Path != nil {
						c.Conversion.Webhook += *svc.Path
					}
				}
			}
		}
	}
	return c, nil
}
