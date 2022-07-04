package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"

	"github.com/aquasecurity/trivy/pkg/cloud/aws/scanner"
	"github.com/aquasecurity/trivy/pkg/cloud/report"

	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	cmd "github.com/aquasecurity/trivy/pkg/commands/artifact"
	"github.com/aquasecurity/trivy/pkg/log"

	awsScanner "github.com/aquasecurity/defsec/pkg/scanners/cloud/aws"
)

// Run runs an aws scan
func Run(cliCtx *cli.Context) error {
	opt, err := cmd.InitOption(cliCtx)
	if err != nil {
		return xerrors.Errorf("option error: %w", err)
	}
	return run(cliCtx.Context, opt)
}

func getAccountID(ctx context.Context) (string, error) {
	log.Logger.Debug("Looking for AWS credentials provider...")
	sess, err := session.NewSession(&aws.Config{
		CredentialsChainVerboseErrors: aws.Bool(true),
	})
	if err != nil {
		return "", fmt.Errorf("failed to create AWS session: %w", err)
	}

	svc := sts.New(sess)

	log.Logger.Debug("Looking up AWS caller identity...")
	result, err := svc.GetCallerIdentityWithContext(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("failed to discover AWS caller identity: %w", err)
	}
	if result.Account == nil {
		return "", fmt.Errorf("missing account id for aws account")
	}
	log.Logger.Debugf("Verified AWS credentials for account %s!", *result.Account)
	return *result.Account, nil
}

func run(ctx context.Context, opt cmd.Option) error {
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	if err := log.InitLogger(opt.Debug, false); err != nil {
		return xerrors.Errorf("logger error: %w", err)
	}

	var err error
	defer func() {
		if xerrors.Is(err, context.DeadlineExceeded) {
			log.Logger.Warn("Increase --timeout value")
		}
	}()

	reportOptions := report.Option{
		Format:     opt.Format,
		Output:     opt.Output,
		Severities: opt.Severities,
	}

	accountID, err := getAccountID(ctx)
	if err != nil {
		return err
	}

	allServices := opt.Services

	if len(allServices) == 0 {
		log.Logger.Debug("No service(s) specified, scanning all services...")
		allServices = awsScanner.AllSupportedServices()
	} else {
		log.Logger.Debugf("Specific services were requested: [%s]...", strings.Join(allServices, ", "))
	}

	log.Logger.Debugf("Attempting to load results from cache...")
	cached, err := report.LoadReport(accountID, allServices)
	if err != nil {
		if err != report.ErrCacheNotFound {
			return err
		}
		log.Logger.Debug("Cached results not found.")
	}

	var remaining []string
	for _, service := range allServices {
		if cached != nil {
			var inCache bool
			for _, cacheSvc := range cached.ServicesInScope {
				if cacheSvc == service {
					log.Logger.Debugf("Results for service '%s' found in cache.", service)
					inCache = true
					break
				}
			}
			if inCache {
				continue
			}
		}
		remaining = append(remaining, service)
	}

	var r *report.Report

	// if there is anything we need that wasn't in the cache, scan it now
	if len(remaining) > 0 {
		log.Logger.Debugf("Scanning non-cached services: [%s]...", strings.Join(remaining, ", "))
		opt.Services = remaining
		results, err := scanner.NewScanner().Scan(ctx, opt)
		if err != nil {
			return xerrors.Errorf("aws scan error: %w", err)
		}
		r = report.New(accountID, results, allServices)
	} else {
		r = report.New(accountID, nil, allServices)
	}
	if cached != nil {
		r.Merge(cached, true)
		reportOptions.FromCache = true
	}

	if err := r.Save(); err != nil {
		return err
	}

	if err := report.Write(r, reportOptions); err != nil {
		return xerrors.Errorf("unable to write results: %w", err)
	}

	cmd.Exit(opt, r.Failed())
	return nil
}
