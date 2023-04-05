package config

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/digitalocean/godo"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/logging"
	"golang.org/x/oauth2"
)

type Config struct {
	Token             string
	APIEndpoint       string
	SpacesAPIEndpoint string
	AccessID          string
	SecretKey         string
	TerraformVersion  string
	HTTPRetryMax      int
	HTTPRetryWaitMax  float64
	HTTPRetryWaitMin  float64
}

type CombinedConfig struct {
	client                 *godo.Client
	spacesEndpointTemplate *template.Template
	accessID               string
	secretKey              string
}

func (c *CombinedConfig) GodoClient() *godo.Client { return c.client }

func (c *CombinedConfig) SpacesClient(region string) (*session.Session, error) {
	if c.accessID == "" || c.secretKey == "" {
		err := fmt.Errorf("Spaces credentials not configured")
		return &session.Session{}, err
	}

	endpointWriter := strings.Builder{}
	err := c.spacesEndpointTemplate.Execute(&endpointWriter, map[string]string{
		"Region": strings.ToLower(region),
	})
	if err != nil {
		return &session.Session{}, err
	}
	endpoint := endpointWriter.String()

	client, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials(c.accessID, c.secretKey, ""),
		Endpoint:    aws.String(endpoint)},
	)
	if err != nil {
		return &session.Session{}, err
	}

	return client, nil
}

// Client() returns a new client for accessing digital ocean.
func (c *Config) Client() (*CombinedConfig, error) {
	tokenSrc := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: c.Token,
	})

	userAgent := fmt.Sprintf("Terraform/%s", c.TerraformVersion)

	retryableClient := retryablehttp.NewClient()
	retryableClient.RetryMax = c.HTTPRetryMax
	retryableClient.RetryWaitMin = time.Duration(c.HTTPRetryWaitMin * float64(time.Second))
	retryableClient.RetryWaitMax = time.Duration(c.HTTPRetryWaitMax * float64(time.Second))
	retryableClient.Backoff = digitalOceanAPIBackoff

	client := retryableClient.StandardClient()
	client.Transport = &oauth2.Transport{
		Base:   client.Transport,
		Source: oauth2.ReuseTokenSource(nil, tokenSrc),
	}

	client.Transport = logging.NewTransport("DigitalOcean", client.Transport)

	godoClient, err := godo.New(client, godo.SetUserAgent(userAgent))
	if err != nil {
		return nil, err
	}

	apiURL, err := url.Parse(c.APIEndpoint)
	if err != nil {
		return nil, err
	}
	godoClient.BaseURL = apiURL

	spacesEndpointTemplate, err := template.New("spaces").Parse(c.SpacesAPIEndpoint)
	if err != nil {
		return nil, fmt.Errorf("unable to parse spaces_endpoint '%s' as template: %s", c.SpacesAPIEndpoint, err)
	}

	log.Printf("[INFO] DigitalOcean Client configured for URL: %s", godoClient.BaseURL.String())

	return &CombinedConfig{
		client:                 godoClient,
		spacesEndpointTemplate: spacesEndpointTemplate,
		accessID:               c.AccessID,
		secretKey:              c.SecretKey,
	}, nil
}

func digitalOceanAPIBackoff(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		// Retrieve API's Rate Limit Reset unix timestamp
		if s, ok := resp.Header["Ratelimit-Reset"]; ok {
			if resetUnix, err := strconv.ParseInt(s[0], 10, 64); err == nil {
				nowUnix := time.Now().Unix()
				sleep := time.Second * time.Duration(resetUnix-nowUnix)

				log.Printf("[INFO] Reached API Rate Limit, waiting: %s seconds", sleep)

				// Cap sleep time to maximum configured value (to prevent too long wait times for mismatched clocks)
				if sleep > max {
					return max
				}

				// Avoid negative sleep times (generally indicates a mismatched clock)
				if sleep > 0 {
					return sleep
				}
			}
		}
	}

	// Fallback to default backoff strategy
	sleep := retryablehttp.DefaultBackoff(min, max, attemptNum, resp)
	log.Printf("[INFO] API Error (not Rate Limit), waiting: %s seconds", sleep)
	return sleep
}
