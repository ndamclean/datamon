package cmd

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"github.com/oneconcern/datamon/pkg/web"

	"github.com/pkg/browser"
	"github.com/spf13/cobra"
)

var webSrv = &cobra.Command{
	Use:   "web",
	Short: "Webserver",
	Long:  "A webserver process to browse datamon data",
	Run: func(cmd *cobra.Command, args []string) {
		infoLogger.Println("begin webserver")
		stores, err := paramsToDatamonContext(context.Background(), datamonFlags)
		if err != nil {
			wrapFatalln("create remote stores", err)
			return
		}
		s, err := web.NewServer(web.ServerParams{
			Stores:     stores,
			Credential: config.Credential,
		})
		if err != nil {
			wrapFatalln("server init error", err)
			return
		}

		listener, err := net.Listen("tcp4", net.JoinHostPort("", strconv.Itoa(datamonFlags.web.port)))
		if err != nil {
			wrapFatalln("listener init error", err)
			return
		}

		r := web.InitRouter(s)

		latch := make(chan struct{})
		errServe := make(chan error)
		go func() {
			webServer := new(http.Server)
			webServer.SetKeepAlivesEnabled(true)
			webServer.Handler = r
			latch <- struct{}{}
			errServe <- webServer.Serve(listener)
		}()

		<-latch
		infoLogger.Printf("serving datamon UI at %s...", listener.Addr().String())

		if !datamonFlags.web.noBrowser {
			err = browser.OpenURL("http://" + listener.Addr().String())
			if err != nil {
				wrapFatalln("cannot launch browser", err)
				return
			}
		}

		err = <-errServe
		if err != nil {
			wrapFatalln("server error", err)
		}
	},
	PreRun: func(cmd *cobra.Command, args []string) {
		config.populateRemoteConfig(&datamonFlags)
	}, // https://github.com/spf13/cobra/issues/458
}

func init() {
	/* web datamonFlags */
	addWebPortFlag(webSrv)
	addWebNoBrowserFlag(webSrv)

	/* core datamonFlags */
	//	addMetadataBucket(repoList)

	rootCmd.AddCommand(webSrv)
}
