// Minimal inert prefs so a headless throwaway profile does not phone home
// or restore anything. TRR (DNS-over-HTTPS) is force-disabled so the
// rebind scenario resolves evil.test through /etc/hosts, like a real
// attacker-controlled DNS would in production.
user_pref("network.trr.mode", 5);
user_pref("network.captive-portal-service.enabled", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("datareporting.healthreport.uploadEnabled", false);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("app.update.auto", false);
user_pref("browser.aboutwelcome.enabled", false);
user_pref("startup.homepage_welcome_url", "");
user_pref("startup.homepage_override_url", "");
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
