package wechat

import (
	"context"
	"fmt"

	ilink "github.com/openilink/openilink-sdk-go"
)

// Login runs the interactive QR-code login: it prints the QR in the terminal, polls until the user
// scans with WeChat, then persists the account credentials.
func Login(ctx context.Context) error {
	logf("Starting WeChat QR-code login...")

	client := ilink.NewClient("", ilink.WithBaseURL(DefaultBaseURL))
	scannedPrinted := false
	result, err := client.LoginWithQR(ctx, &ilink.LoginCallbacks{
		OnQRCode: func(content string) {
			logf("\nScan the QR code below with WeChat to log in:\n")
			printTerminalQR(content)
			logf("\nWaiting for scan...\n")
		},
		OnScanned: func() {
			if !scannedPrinted {
				fmt.Print("\nScanned. Please confirm in WeChat...\n")
				scannedPrinted = true
			}
		},
		OnExpired: func(attempt, max int) {
			logf("\nQR code expired, refreshing... (%d/%d)\n", attempt, max)
		},
	})
	if err != nil {
		return fmt.Errorf("wechat login: %w", err)
	}
	if !result.Connected {
		if result.Message == "login timeout" {
			return fmt.Errorf("login timed out (no scan within 8 minutes), please retry: botler wechat-login")
		}
		return fmt.Errorf("login incomplete (%s), please retry: botler wechat-login", result.Message)
	}

	SaveAccount(AccountUpdate{Token: result.BotToken, BaseURL: result.BaseURL, UserID: result.UserID})
	owner := result.UserID
	if owner == "" {
		owner = "(none)"
	}
	logf("\nConnected to WeChat successfully!\n   bot_id: %s\n   owner userId: %s", result.BotID, owner)
	return nil
}
