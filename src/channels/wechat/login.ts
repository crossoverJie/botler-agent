import { apiGetFetch } from "./api.ts";
import { saveAccount } from "./account.ts";

const FIXED_BASE_URL = "https://ilinkai.weixin.qq.com";
const DEFAULT_ILINK_BOT_TYPE = "3";
const GET_QRCODE_TIMEOUT_MS = 5_000;
const QR_LONG_POLL_TIMEOUT_MS = 35_000;
/** Max QR-code refreshes before giving up (matches the SDK's MAX_QR_REFRESH_COUNT). */
const MAX_QR_REFRESH_COUNT = 3;

interface QRCodeResponse {
	qrcode: string;
	qrcode_img_content: string;
}

interface StatusResponse {
	status: "wait" | "scaned" | "confirmed" | "expired" | "scaned_but_redirect";
	bot_token?: string;
	ilink_bot_id?: string;
	baseurl?: string;
	ilink_user_id?: string;
	redirect_host?: string;
}

async function fetchQRCode(apiBaseUrl: string, botType: string): Promise<QRCodeResponse> {
	const rawText = await apiGetFetch({
		baseUrl: apiBaseUrl,
		endpoint: `ilink/bot/get_bot_qrcode?bot_type=${encodeURIComponent(botType)}`,
		timeoutMs: GET_QRCODE_TIMEOUT_MS,
		label: "fetchQRCode",
	});
	return JSON.parse(rawText) as QRCodeResponse;
}

async function pollQRStatus(apiBaseUrl: string, qrcode: string): Promise<StatusResponse> {
	try {
		const rawText = await apiGetFetch({
			baseUrl: apiBaseUrl,
			endpoint: `ilink/bot/get_qrcode_status?qrcode=${encodeURIComponent(qrcode)}`,
			timeoutMs: QR_LONG_POLL_TIMEOUT_MS,
			label: "pollQRStatus",
		});
		return JSON.parse(rawText) as StatusResponse;
	} catch (err) {
		if (err instanceof Error && err.name === "AbortError") {
			return { status: "wait" };
		}
		// Network/gateway error → treat as still-waiting and keep polling
		console.warn(`[wechat] pollQRStatus network error, retry: ${String(err)}`);
		return { status: "wait" };
	}
}

/** Render the QR content in the terminal; fall back to printing the URL. */
async function printQr(qrcodeContent: string): Promise<void> {
	try {
		const qrcodeterminal = await import("qrcode-terminal");
		await new Promise<void>((resolve) => {
			qrcodeterminal.default.generate(qrcodeContent, { small: true }, (qr: string) => {
				console.log(qr);
				resolve();
			});
		});
	} catch {
		console.log(`QR content (open in a browser): ${qrcodeContent}`);
	}
}

/**
 * Interactive QR-code login. Prints a QR code, polls until the user scans with
 * WeChat, then persists the account credentials. Returns after success.
 */
export async function wechatLogin(): Promise<void> {
	console.log("Starting WeChat QR-code login...");
	const initial = await fetchQRCode(FIXED_BASE_URL, DEFAULT_ILINK_BOT_TYPE);
	let qrcode = initial.qrcode;
	let currentBaseUrl = FIXED_BASE_URL;
	let scannedPrinted = false;
	let qrRefreshCount = 0;

	console.log("\nScan the QR code below with WeChat to log in:\n");
	await printQr(initial.qrcode_img_content);
	console.log("\nWaiting for scan...\n");

	const deadline = Date.now() + 480_000;
	while (Date.now() < deadline) {
		const statusResponse = await pollQRStatus(currentBaseUrl, qrcode);
		switch (statusResponse.status) {
			case "wait":
				break;
			case "scaned":
				if (!scannedPrinted) {
					process.stdout.write("\nScanned. Please confirm in WeChat...\n");
					scannedPrinted = true;
				}
				break;
			case "expired": {
				qrRefreshCount += 1;
				if (qrRefreshCount > MAX_QR_REFRESH_COUNT) {
					throw new Error(
						`QR code expired ${MAX_QR_REFRESH_COUNT} times; please re-run: npm start -- wechat-login`,
					);
				}
				console.log(`\nQR code expired, refreshing... (${qrRefreshCount}/${MAX_QR_REFRESH_COUNT})\n`);
				const fresh = await fetchQRCode(FIXED_BASE_URL, DEFAULT_ILINK_BOT_TYPE);
				qrcode = fresh.qrcode;
				currentBaseUrl = FIXED_BASE_URL;
				scannedPrinted = false;
				await printQr(fresh.qrcode_img_content);
				console.log("\nPlease scan again...\n");
				break;
			}
			case "scaned_but_redirect": {
				if (statusResponse.redirect_host) {
					currentBaseUrl = `https://${statusResponse.redirect_host}`;
					console.log(`[wechat] IDC redirect to ${statusResponse.redirect_host}`);
				}
				break;
			}
			case "confirmed": {
				if (!statusResponse.bot_token || !statusResponse.ilink_bot_id) {
					throw new Error("Login confirmed but server did not return token / bot id");
				}
				saveAccount({
					token: statusResponse.bot_token,
					baseUrl: statusResponse.baseurl,
					userId: statusResponse.ilink_user_id,
				});
				console.log(
					`\nConnected to WeChat successfully!\n   bot_id: ${statusResponse.ilink_bot_id}\n   owner userId: ${statusResponse.ilink_user_id ?? "(none)"}`,
				);
				return;
			}
		}
		await new Promise((r) => setTimeout(r, 1000));
	}
	throw new Error("Login timed out (no scan within 8 minutes), please retry");
}
