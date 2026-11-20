package aws

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/paingha/tugcli/pkg/provider"
)

// fakeEKS serves ListClusters in pages and DescribeCluster from a map.
type fakeEKS struct {
	clusters map[string]*ekstypes.Cluster
	pageSize int
	err      error
}

func (f *fakeEKS) ListClusters(_ context.Context, in *eks.ListClustersInput, _ ...func(*eks.Options)) (*eks.ListClustersOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	var names []string
	for n := range f.clusters {
		names = append(names, n)
	}
	slices.Sort(names)
	start := 0
	if in.NextToken != nil {
		for i, n := range names {
			if n == *in.NextToken {
				start = i
			}
		}
	}
	end := min(start+f.pageSize, len(names))
	out := &eks.ListClustersOutput{Clusters: names[start:end]}
	if end < len(names) {
		out.NextToken = awsv2.String(names[end])
	}
	return out, nil
}

func (f *fakeEKS) DescribeCluster(_ context.Context, in *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	return &eks.DescribeClusterOutput{Cluster: f.clusters[*in.Name]}, nil
}

func staticPresigner() Presigner {
	client := sts.New(sts.Options{
		Region:      "us-east-1",
		Credentials: awsv2.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")),
	})
	return sts.NewPresignClient(client)
}

func cluster(name string, public bool, tags map[string]string) *ekstypes.Cluster {
	return &ekstypes.Cluster{
		Name:     awsv2.String(name),
		Arn:      awsv2.String("arn:aws:eks:eu-west-1:123456789012:cluster/" + name),
		Endpoint: awsv2.String("https://" + name + ".gr7.eu-west-1.eks.amazonaws.com"),
		Version:  awsv2.String("1.34"),
		CertificateAuthority: &ekstypes.Certificate{
			Data: awsv2.String(base64.StdEncoding.EncodeToString([]byte("-----BEGIN CERTIFICATE-----"))),
		},
		ResourcesVpcConfig: &ekstypes.VpcConfigResponse{EndpointPublicAccess: public},
		Tags:               tags,
	}
}

func newTestProvider(fake *fakeEKS, regions map[string]string) (*Provider, *[]string) {
	var calls []string
	p := New(
		WithClients(func(_ context.Context, profile, region string) (EKSAPI, Presigner, error) {
			calls = append(calls, profile+"@"+region)
			return fake, staticPresigner(), nil
		}),
		WithNow(func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }),
	)
	p.defaultRegion = func(_ context.Context, profile string) string { return regions[profile] }
	return p, &calls
}

func TestListAcrossRegionsAndPages(t *testing.T) {
	creating := cluster("creating", true, nil)
	creating.Endpoint, creating.CertificateAuthority = nil, nil // not ACTIVE yet
	fake := &fakeEKS{pageSize: 1, clusters: map[string]*ekstypes.Cluster{
		"payments-prod": cluster("payments-prod", true, map[string]string{"Environment": "prod", "team": "pay"}),
		"internal":      cluster("internal", false, nil),
		"creating":      creating,
	}}
	p, calls := newTestProvider(fake, nil)
	acct := provider.Account{ID: "work", Config: map[string]any{ConfigProfile: "work", ConfigRegions: "eu-west-1, us-east-1,eu-west-1"}}

	clusters, err := p.List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 4 { // two ready clusters in each of two regions; "creating" is skipped
		t.Fatalf("got %d clusters", len(clusters))
	}
	if strings.Join(*calls, ",") != "work@eu-west-1,work@us-east-1" {
		t.Fatalf("regions queried: %v", *calls)
	}

	byID := map[string]provider.Cluster{}
	for _, c := range clusters {
		byID[c.ID] = c
	}
	prod := byID["eu-west-1/payments-prod"]
	if prod.Endpoints.Public == "" || prod.Endpoints.Private != "" || string(prod.CAData) != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("public cluster: %+v", prod)
	}
	if prod.Labels["env"] != "prod" || prod.Labels["team"] != "pay" || prod.Labels["aws-account-id"] != "123456789012" {
		t.Fatalf("labels: %v", prod.Labels)
	}
	if internal := byID["eu-west-1/internal"]; internal.Endpoints.Private == "" || internal.Endpoints.Public != "" {
		t.Fatalf("private-only cluster: %+v", internal.Endpoints)
	}
}

func TestRegionFallbackAndMissingRegion(t *testing.T) {
	fake := &fakeEKS{pageSize: 10, clusters: map[string]*ekstypes.Cluster{}}
	p, calls := newTestProvider(fake, map[string]string{"dev": "ap-south-1"})

	if _, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "dev"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*calls, ",") != "dev@ap-south-1" {
		t.Fatalf("expected the profile's region, queried %v", *calls)
	}

	_, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "nowhere"}})
	if provider.CodeOf(err) != provider.InvalidConfig || !strings.Contains(provider.AsError(err).Hint, "--set regions=") {
		t.Fatalf("missing region: %v", err)
	}
}

func TestCredentialIsAnEKSToken(t *testing.T) {
	p, _ := newTestProvider(&fakeEKS{}, nil)
	c, err := p.Credential(context.Background(), provider.CredentialRequest{
		Account: provider.Account{ID: "work"}, ClusterID: "eu-west-1/payments-prod",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Type != provider.CredentialToken || !strings.HasPrefix(c.Token, "k8s-aws-v1.") {
		t.Fatalf("credential: %+v", c)
	}
	if want := time.Date(2026, 11, 20, 12, 14, 0, 0, time.UTC); !c.ExpiresAt.Equal(want) {
		t.Fatalf("expires %s, want %s", c.ExpiresAt, want)
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c.Token, "k8s-aws-v1."))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("Action") != "GetCallerIdentity" || q.Get("X-Amz-Expires") != "60" {
		t.Fatalf("presigned URL: %s", raw)
	}
	if !strings.Contains(q.Get("X-Amz-SignedHeaders"), "x-k8s-aws-id") {
		t.Fatalf("cluster header not signed: %s", q.Get("X-Amz-SignedHeaders"))
	}

	if _, err := p.Credential(context.Background(), provider.CredentialRequest{ClusterID: "no-region"}); provider.CodeOf(err) != provider.InvalidConfig {
		t.Fatalf("bad cluster id: %v", err)
	}
}

func TestAccountsFromAWSConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".aws")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config"), []byte(`
[default]
region = us-east-1

[profile work]
sso_session = corp
region = eu-west-1

[sso-session corp]
sso_region = eu-west-1
`), 0o600)
	os.WriteFile(filepath.Join(dir, "credentials"), []byte("[ci]\naws_access_key_id = x\n"), 0o600)

	p := New(WithHome(home), WithGetenv(func(string) string { return "" }))
	accts, err := p.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range accts {
		got = append(got, a.ID+":"+a.ConfigString(ConfigRegions))
	}
	if strings.Join(got, ",") != "ci:,default:us-east-1,work:eu-west-1" {
		t.Fatalf("accounts = %v", got)
	}
}

type apiErr struct{ code, msg string }

func (e apiErr) Error() string                 { return e.code + ": " + e.msg }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.msg }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestAWSErrorMapping(t *testing.T) {
	tests := []struct {
		err  error
		want provider.Code
	}{
		{apiErr{"UnrecognizedClientException", "bad token"}, provider.Unauthenticated},
		{apiErr{"ExpiredTokenException", "expired"}, provider.Unauthenticated},
		{apiErr{"AccessDeniedException", "no"}, provider.PermissionDenied},
		{apiErr{"ThrottlingException", "slow"}, provider.RateLimited},
		{apiErr{"ResourceNotFoundException", "gone"}, provider.NotFound},
		{&credentialsError{err: errors.New("the SSO session has expired")}, provider.Unauthenticated},
		{fmt.Errorf("listing: %w", &credentialsError{err: errors.New("no EC2 IMDS role found")}), provider.Unauthenticated},
		{awsconfig.SharedConfigProfileNotExistError{Profile: "gone"}, provider.NotFound},
		{errors.New("dial tcp: no such host"), provider.Unavailable},
	}
	for _, tt := range tests {
		if got := provider.CodeOf(awsError(tt.err, "work")); got != tt.want {
			t.Errorf("awsError(%v) = %s, want %s", tt.err, got, tt.want)
		}
	}
	fake := &fakeEKS{err: apiErr{"UnrecognizedClientException", "bad"}}
	p, _ := newTestProvider(fake, nil)
	_, err := p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "work", Config: map[string]any{ConfigRegions: "eu-west-1"}}})
	if provider.CodeOf(err) != provider.Unauthenticated || !strings.Contains(provider.AsError(err).Hint, "aws sso login --profile work") {
		t.Fatalf("list with bad credentials: %v", err)
	}
}
