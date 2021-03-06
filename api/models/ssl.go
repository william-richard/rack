package models

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/acm"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/iam"
)

type SSL struct {
	Certificate string    `json:"certificate"`
	Expiration  time.Time `json:"expiration"`
	Domain      string    `json:"domain"`
	Process     string    `json:"process"`
	Port        int       `json:"port"`
	Secure      bool      `json:"secure"`
}

type SSLs []SSL

func ListSSLs(a string) (SSLs, error) {
	app, err := GetApp(a)

	if err != nil {
		return nil, err
	}

	ssls := make(SSLs, 0)

	// Find stack Parameters like WebPort443Listener or WebPort443Certificate with an ARN set for the value
	// Get and decode corresponding certificate info
	cRe := regexp.MustCompile(`(\w+)Port(\d+)Certificate`)
	lRe := regexp.MustCompile(`(\w+)Port(\d+)Listener`)

	for k, v := range app.Parameters {
		matches := []string{}
		arn := ""

		if ms := cRe.FindStringSubmatch(k); len(ms) > 0 {
			matches = ms
			arn = v
		} else if ms := lRe.FindStringSubmatch(k); len(ms) > 0 {
			matches = ms

			parts := strings.Split(v, ",")
			if len(parts) != 2 {
				return nil, fmt.Errorf("%s not in Port,Cert format", k)
			}

			arn = parts[1]
		}

		if arn == "" {
			continue
		}

		if len(matches) > 0 {
			port, err := strconv.Atoi(matches[2])
			if err != nil {
				return nil, err
			}

			secure := app.Parameters[fmt.Sprintf("%sPort%sSecure", matches[1], matches[2])] == "Yes"

			switch prefix := arn[8:11]; prefix {
			case "acm":
				res, err := ACM().DescribeCertificate(&acm.DescribeCertificateInput{
					CertificateArn: aws.String(arn),
				})

				if err != nil {
					return nil, err
				}

				parts := strings.Split(arn, "-")
				id := fmt.Sprintf("acm-%s", parts[len(parts)-1])

				ssls = append(ssls, SSL{
					Certificate: id,
					Domain:      *res.Certificate.DomainName,
					Expiration:  *res.Certificate.NotAfter,
					Port:        port,
					Process:     DashName(matches[1]),
					Secure:      secure,
				})
			case "iam":
				res, err := IAM().GetServerCertificate(&iam.GetServerCertificateInput{
					ServerCertificateName: aws.String(certName(app.StackName(), matches[1], port)),
				})

				if err != nil {
					return nil, err
				}

				pemBlock, _ := pem.Decode([]byte(*res.ServerCertificate.CertificateBody))

				c, err := x509.ParseCertificate(pemBlock.Bytes)

				if err != nil {
					return nil, err
				}

				ssls = append(ssls, SSL{
					Certificate: *res.ServerCertificate.ServerCertificateMetadata.ServerCertificateName,
					Domain:      c.Subject.CommonName,
					Expiration:  *res.ServerCertificate.ServerCertificateMetadata.Expiration,
					Port:        port,
					Process:     DashName(matches[1]),
					Secure:      secure,
				})
			default:
				return nil, fmt.Errorf("unknown arn prefix: %s", prefix)
			}
		}
	}

	return ssls, nil
}

func UpdateSSL(app, process string, port int, id string) (*SSL, error) {
	a, err := GetApp(app)

	if err != nil {
		return nil, err
	}

	// validate app is not currently updating
	if a.Status != "running" {
		return nil, fmt.Errorf("can not update app with status: %s", a.Status)
	}

	outputs := a.Outputs
	balancer := outputs[fmt.Sprintf("%sPort%dBalancerName", UpperName(process), port)]

	if balancer == "" {
		return nil, fmt.Errorf("Process and port combination unknown")
	}

	arn := ""

	if strings.HasPrefix(id, "acm-") {
		uuid := id[4:]

		res, err := ACM().ListCertificates(nil)

		if err != nil {
			return nil, err
		}

		for _, cert := range res.CertificateSummaryList {
			parts := strings.Split(*cert.CertificateArn, "-")

			if parts[len(parts)-1] == uuid {
				res, err := ACM().DescribeCertificate(&acm.DescribeCertificateInput{
					CertificateArn: cert.CertificateArn,
				})

				if err != nil {
					return nil, err
				}

				if *res.Certificate.Status == "PENDING_VALIDATION" {
					return nil, fmt.Errorf("%s is still pending validation", id)
				}

				arn = *cert.CertificateArn
				break
			}
		}
	} else {
		res, err := IAM().GetServerCertificate(&iam.GetServerCertificateInput{
			ServerCertificateName: aws.String(id),
		})

		if err != nil {
			return nil, err
		}

		arn = *res.ServerCertificate.ServerCertificateMetadata.Arn
	}

	// update cloudformation
	req := &cloudformation.UpdateStackInput{
		StackName:           aws.String(a.StackName()),
		Capabilities:        []*string{aws.String("CAPABILITY_IAM")},
		UsePreviousTemplate: aws.Bool(true),
		NotificationARNs:    []*string{aws.String(cloudformationTopic)},
	}

	certParam := fmt.Sprintf("%sPort%dCertificate", UpperName(process), port)
	listenerParam := fmt.Sprintf("%sPort%dListener", UpperName(process), port)

	params := a.Parameters

	if _, ok := params[certParam]; ok {
		params[certParam] = arn
	}

	if v, ok := params[listenerParam]; ok {
		parts := strings.Split(v, ",")
		parts[1] = arn
		params[listenerParam] = strings.Join(parts, ",")
	}

	for key, val := range params {
		req.Parameters = append(req.Parameters, &cloudformation.Parameter{
			ParameterKey:   aws.String(key),
			ParameterValue: aws.String(val),
		})
	}

	// TODO: The existing cert will be orphaned. Deleting it now could cause
	// CF problems if the stack tries to rollback and use the old cert.
	_, err = UpdateStack(req)

	if err != nil {
		return nil, err
	}

	ssl := SSL{
		Port:    port,
		Process: process,
	}

	return &ssl, nil
}

// fetch certificate from CF params and parse name from arn
func certName(app, process string, port int) string {
	a, err := GetApp(app)
	if err != nil {
		fmt.Printf(err.Error())
		return ""
	}

	certParam := fmt.Sprintf("%sPort%dCertificate", UpperName(process), port)
	listenerParam := fmt.Sprintf("%sPort%dListener", UpperName(process), port)

	arn := ""

	if v, ok := a.Parameters[certParam]; ok {
		arn = v
	}

	if v, ok := a.Parameters[listenerParam]; ok {
		parts := strings.Split(v, ",")
		if len(parts) != 2 {
			return ""
		}
		arn = parts[1]
	}

	slice := strings.Split(arn, "/")

	return slice[len(slice)-1]
}
