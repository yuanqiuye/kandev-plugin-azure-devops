import { currentHost, useEffect, useState } from "../runtime";
import { CardContent, Label, SettingsCard, SettingsSection, Switch } from "../host-ui";
import { useT } from "../i18n";
import { getAzureDevOpsWorkspaceSettings, updateAzureDevOpsWorkspaceSettings } from "../browse/rest-compat";

export function AzureDevOpsAutoFixSettings({ workspaceId }: { workspaceId: string }) {
  const t = useT();
  const host = currentHost();
  const [enabledByDefault, setEnabledByDefault] = useState(true);
  const [requireApprovalByDefault, setRequireApprovalByDefault] = useState(true);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    getAzureDevOpsWorkspaceSettings(workspaceId, { init: { signal: controller.signal } })
      .then((settings) => {
        setEnabledByDefault(settings.autoFixEnabledByDefault ?? true);
        setRequireApprovalByDefault(settings.autoFixRequireApprovalByDefault ?? true);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [workspaceId]);

  const patch = async (next: { autoFixEnabledByDefault?: boolean; autoFixRequireApprovalByDefault?: boolean }) => {
    setSaving(true);
    try {
      const updated = await updateAzureDevOpsWorkspaceSettings(workspaceId, next);
      setEnabledByDefault(updated.autoFixEnabledByDefault ?? true);
      setRequireApprovalByDefault(updated.autoFixRequireApprovalByDefault ?? true);
      host.toast.success(t("autoFixSettingsSaved"));
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      host.toast.error(t("requestFailed", { error: message }));
    } finally {
      setSaving(false);
    }
  };

  return (
    <SettingsSection title={t("autoFixSettingsTitle")} description={t("autoFixSettingsDescription")}>
      <SettingsCard>
        <CardContent className="space-y-4 pt-6">
          <div className="flex items-center justify-between gap-3">
            <div className="space-y-1">
              <Label htmlFor="azure-devops-autofix-default-enabled">{t("autoFixDefaultEnabledLabel")}</Label>
              <p className="text-xs text-muted-foreground">{t("autoFixDefaultEnabledHelp")}</p>
            </div>
            <Switch
              id="azure-devops-autofix-default-enabled"
              data-testid="azure-devops-autofix-default-enabled"
              checked={enabledByDefault}
              disabled={saving}
              onCheckedChange={(checked) => void patch({ autoFixEnabledByDefault: checked })}
            />
          </div>
          <div className="flex items-center justify-between gap-3">
            <div className="space-y-1">
              <Label htmlFor="azure-devops-autofix-default-approval">{t("autoFixDefaultApprovalLabel")}</Label>
              <p className="text-xs text-muted-foreground">{t("autoFixDefaultApprovalHelp")}</p>
            </div>
            <Switch
              id="azure-devops-autofix-default-approval"
              data-testid="azure-devops-autofix-default-approval"
              checked={requireApprovalByDefault}
              disabled={saving}
              onCheckedChange={(checked) => void patch({ autoFixRequireApprovalByDefault: checked })}
            />
          </div>
        </CardContent>
      </SettingsCard>
    </SettingsSection>
  );
}
