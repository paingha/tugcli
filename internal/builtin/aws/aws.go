// Package aws is the built-in provider for Amazon EKS. It lists clusters per
// AWS profile and region, and mints the short-lived token EKS accepts from a
// presigned STS GetCallerIdentity request, so the AWS CLI is not needed.
package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/paingha/tugcli/internal/ini"
	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Name of the provider.
const Name = "aws"

// Account config keys.
const (
	ConfigProfile = "profile"
	ConfigRegions = "regions" // comma-separated
)

// Token details shared with aws-iam-authenticator and `aws eks get-token`.
const (
	tokenPrefix     = "k8s-aws-v1."
	clusterIDHeader = "x-k8s-aws-id"
	// The presigned URL is valid for 15 minutes; tug renews a minute early.
	tokenLifetime = 14 * time.Minute
)

// EKSAPI is the part of the EKS client the provider uses.
type EKSAPI interface {
	ListClusters(ctx context.Context, in *eks.ListClustersInput, opts ...func(*eks.Options)) (*eks.ListClustersOutput, error)
	DescribeCluster(ctx context.Context, in *eks.DescribeClusterInput, opts ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

// Presigner presigns STS GetCallerIdentity requests.
type Presigner interface {
	PresignGetCallerIdentity(ctx context.Context, in *sts.GetCallerIdentityInput, opts ...func(*sts.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// Clients builds the AWS clients for a profile and region.
type Clients func(ctx context.Context, profile, region string) (EKSAPI, Presigner, error)

// Provider lists EKS clusters.
type Provider struct {
	clients Clients
	getenv  func(string) string
	home    string
	now     func() time.Time
	// defaultRegion reports the region a profile is configured with.
	defaultRegion func(ctx context.Context, profile string) string
}

// Option configures a Provider.
type Option func(*Provider)

// WithClients replaces the AWS SDK clients, for tests.
func WithClients(c Clients) Option { return func(p *Provider) { p.clients = c } }

// WithHome sets the home directory used to find ~/.aws.
func WithHome(dir string) Option { return func(p *Provider) { p.home = dir } }

// WithGetenv replaces os.Getenv, for tests.
func WithGetenv(f func(string) string) Option { return func(p *Provider) { p.getenv = f } }

// WithNow replaces time.Now, for tests.
func WithNow(f func() time.Time) Option { return func(p *Provider) { p.now = f } }

// New returns the EKS provider.
func New(opts ...Option) *Provider {
	p := &Provider{clients: sdkClients, getenv: os.Getenv, now: time.Now, defaultRegion: sdkRegion}
	for _, o := range opts {
		o(p)
	}
	if p.home == "" {
		p.home, _ = os.UserHomeDir()
	}
	return p
}

func loadConfig(ctx context.Context, profile, region string) (awsv2.Config, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(profile))
	}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	opts = append(opts, awsconfig.WithAppID("tug/"+version.String()))
	return awsconfig.LoadDefaultConfig(ctx, opts...)
}

func sdkClients(ctx context.Context, profile, region string) (EKSAPI, Presigner, error) {
	cfg, err := loadConfig(ctx, profile, region)
	if err != nil {
		return nil, nil, err
	}
	return eks.NewFromConfig(cfg), sts.NewPresignClient(sts.NewFromConfig(cfg)), nil
}

func sdkRegion(ctx context.Context, profile string) string {
	cfg, err := loadConfig(ctx, profile, "")
	if err != nil {
		return ""
	}
	return cfg.Region
}

// Info implements registry.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      version.String(),
		Description:  "Amazon EKS clusters, per AWS profile and region",
		Capabilities: provider.CapAccounts | provider.CapList | provider.CapCredential,
	}
}

// Accounts returns one account per profile in ~/.aws/config and
// ~/.aws/credentials (or AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE).
func (p *Provider) Accounts(_ context.Context) ([]provider.Account, error) {
	configPath := p.getenv("AWS_CONFIG_FILE")
	if configPath == "" {
		configPath = filepath.Join(p.home, ".aws", "config")
	}
	credsPath := p.getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credsPath == "" {
		credsPath = filepath.Join(p.home, ".aws", "credentials")
	}
	cfg, err := ini.ParseFile(configPath)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading %s: %v", configPath, err)
	}
	creds, err := ini.ParseFile(credsPath)
	if err != nil {
		return nil, provider.Errorf(provider.InvalidConfig, "reading %s: %v", credsPath, err)
	}

	regions := map[string]string{}
	for section := range cfg {
		name, ok := profileName(section)
		if !ok {
			continue
		}
		regions[name] = cfg.Get(section, "region")
	}
	for section := range creds {
		if _, ok := regions[section]; !ok {
			regions[section] = ""
		}
	}
	names := make([]string, 0, len(regions))
	for n := range regions {
		names = append(names, n)
	}
	sort.Strings(names)

	accounts := make([]provider.Account, 0, len(names))
	for _, n := range names {
		acct := provider.Account{ID: n, Description: "AWS profile " + n, Config: map[string]any{ConfigProfile: n}}
		if r := regions[n]; r != "" {
			acct.Config[ConfigRegions] = r
		}
		accounts = append(accounts, acct)
	}
	return accounts, nil
}

// profileName extracts a profile from an ~/.aws/config section header:
// "default" or "profile name". Sections such as "sso-session x" are skipped.
func profileName(section string) (string, bool) {
	if section == "default" {
		return section, true
	}
	if name, ok := strings.CutPrefix(section, "profile "); ok {
		return strings.TrimSpace(name), true
	}
	return "", false
}

func (p *Provider) profile(acct provider.Account) string {
	if v := acct.ConfigString(ConfigProfile); v != "" {
		return v
	}
	return acct.ID
}

// regions returns the account's regions, or the profile's configured region.
func (p *Provider) regions(ctx context.Context, acct provider.Account) ([]string, error) {
	var out []string
	switch v := acct.Config[ConfigRegions].(type) {
	case string:
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
	case []any:
		for _, r := range v {
			if s, ok := r.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		if r := p.defaultRegion(ctx, p.profile(acct)); r != "" {
			out = []string{r}
		}
	}
	if len(out) == 0 {
		return nil, provider.Errorf(provider.InvalidConfig, "no region for AWS account %q", acct.ID).
			WithHint(fmt.Sprintf("tug import aws --account %s --set regions=us-east-1,eu-west-1", acct.ID))
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// List returns the EKS clusters in every region of the account.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	regions, err := p.regions(ctx, req.Account)
	if err != nil {
		return nil, err
	}
	var out []provider.Cluster
	for _, region := range regions {
		api, _, err := p.clients(ctx, p.profile(req.Account), region)
		if err != nil {
			return nil, awsError(err, req.Account.ID)
		}
		names, err := listClusterNames(ctx, api)
		if err != nil {
			return nil, awsError(err, req.Account.ID)
		}
		for _, name := range names {
			desc, err := api.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: awsv2.String(name)})
			if err != nil {
				return nil, awsError(err, req.Account.ID)
			}
			c, err := toCluster(region, desc)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func listClusterNames(ctx context.Context, api EKSAPI) ([]string, error) {
	var names []string
	var next *string
	for {
		page, err := api.ListClusters(ctx, &eks.ListClustersInput{NextToken: next})
		if err != nil {
			return nil, err
		}
		names = append(names, page.Clusters...)
		if page.NextToken == nil || *page.NextToken == "" {
			return names, nil
		}
		next = page.NextToken
	}
}

func toCluster(region string, desc *eks.DescribeClusterOutput) (provider.Cluster, error) {
	c := desc.Cluster
	if c == nil || c.Name == nil {
		return provider.Cluster{}, provider.Errorf(provider.Internal, "DescribeCluster returned no cluster")
	}
	name := *c.Name
	out := provider.Cluster{
		ID:      region + "/" + name,
		Name:    name,
		Region:  region,
		Version: awsv2.ToString(c.Version),
		Labels:  map[string]string{},
	}
	endpoint := awsv2.ToString(c.Endpoint)
	if vpc := c.ResourcesVpcConfig; vpc != nil && !vpc.EndpointPublicAccess {
		out.Endpoints.Private = endpoint
	} else {
		out.Endpoints.Public = endpoint
	}
	if c.CertificateAuthority != nil && c.CertificateAuthority.Data != nil {
		ca, err := base64.StdEncoding.DecodeString(*c.CertificateAuthority.Data)
		if err != nil {
			return provider.Cluster{}, provider.Errorf(provider.Internal, "cluster %s: bad certificate authority data: %v", name, err)
		}
		out.CAData = ca
	}
	for k, v := range c.Tags {
		key := strings.ToLower(k)
		if key == "environment" {
			key = "env"
		}
		out.Labels[key] = v
	}
	if c.Arn != nil {
		if parts := strings.Split(*c.Arn, ":"); len(parts) > 4 && parts[4] != "" {
			out.Labels["aws-account-id"] = parts[4]
		}
	}
	return out, nil
}

// Credential mints an EKS token for the cluster: a presigned STS
// GetCallerIdentity URL that names the cluster in a signed header.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	region, name, ok := strings.Cut(req.ClusterID, "/")
	if !ok || region == "" || name == "" {
		return provider.Credential{}, provider.Errorf(provider.InvalidConfig, "cluster id %q is not region/name", req.ClusterID)
	}
	_, presigner, err := p.clients(ctx, p.profile(req.Account), region)
	if err != nil {
		return provider.Credential{}, awsError(err, req.Account.ID)
	}
	token, err := Token(ctx, presigner, name)
	if err != nil {
		return provider.Credential{}, awsError(err, req.Account.ID)
	}
	return provider.Token(token, p.now().Add(tokenLifetime)), nil
}

// Token returns an EKS bearer token for a cluster name.
func Token(ctx context.Context, presigner Presigner, clusterName string) (string, error) {
	req, err := presigner.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}, func(po *sts.PresignOptions) {
		po.ClientOptions = append(po.ClientOptions, func(o *sts.Options) {
			o.APIOptions = append(o.APIOptions,
				smithyhttp.SetHeaderValue(clusterIDHeader, clusterName),
				smithyhttp.SetHeaderValue("X-Amz-Expires", "60"),
			)
		})
	})
	if err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(req.URL)), nil
}

// awsError maps AWS SDK errors to protocol codes.
func awsError(err error, account string) error {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "UnrecognizedClientException", "InvalidClientTokenId", "ExpiredToken", "ExpiredTokenException", "InvalidSignatureException":
			return provider.Errorf(provider.Unauthenticated, "%s", api.ErrorMessage()).WithHint(signInHint(account))
		case "AccessDeniedException", "AccessDenied", "UnauthorizedOperation":
			return provider.Errorf(provider.PermissionDenied, "%s", api.ErrorMessage())
		case "ResourceNotFoundException":
			return provider.Errorf(provider.NotFound, "%s", api.ErrorMessage())
		case "ThrottlingException", "TooManyRequestsException", "Throttling":
			return provider.Errorf(provider.RateLimited, "%s", api.ErrorMessage())
		case "ServiceUnavailableException", "ServerException":
			return provider.Errorf(provider.Unavailable, "%s", api.ErrorMessage())
		}
		return provider.Errorf(provider.Internal, "%s: %s", api.ErrorCode(), api.ErrorMessage())
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "failed to retrieve credentials"), strings.Contains(msg, "no EC2 IMDS role found"),
		strings.Contains(msg, "SSO session"), strings.Contains(msg, "token has expired"), strings.Contains(msg, "refresh cached SSO token"):
		return provider.Errorf(provider.Unauthenticated, "%s", msg).WithHint(signInHint(account))
	case strings.Contains(msg, "failed to get shared config profile"):
		return provider.Errorf(provider.NotFound, "%s", msg)
	}
	return provider.Errorf(provider.Unavailable, "%s", msg)
}

func signInHint(account string) string {
	return fmt.Sprintf("sign in with `aws sso login --profile %s` or refresh the profile's credentials", account)
}
