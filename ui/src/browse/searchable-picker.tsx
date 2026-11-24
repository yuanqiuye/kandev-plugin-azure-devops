import { useMemo, useState } from "../runtime";
import { IconCheck, IconChevronDown } from "../icons";
import { Button, Combobox, Input, Popover, PopoverContent, PopoverTrigger } from "../host-ui";
import { cn } from "../cn";
import { useT } from "../i18n";
import { pickedValue, pickerFilter } from "./search-query";

export type PickerOption = { id: string; name: string; description?: string };

/**
 * Above this threshold, cmdk's unvirtualized DOM list lags on open and typing
 * (large Azure DevOps projects can have 6,000+ teams or 20,000+ repositories).
 */
const LARGE_PICKER_THRESHOLD = 300;
const MAX_VISIBLE_OPTIONS = 100;

function LargeSearchablePicker({
  value,
  options,
  onChange,
  placeholder,
  testId,
  triggerId,
  className,
  disabled,
}: {
  value: string;
  options: PickerOption[];
  onChange: (value: string) => void;
  placeholder: string;
  testId?: string;
  triggerId?: string;
  className?: string;
  disabled?: boolean;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const selected = useMemo(() => options.find((option) => option.id === value), [options, value]);
  const visible = useMemo(() => {
    const query = search.trim().toLowerCase();
    const matches: PickerOption[] = [];
    if (selected && (!query || selected.name.toLowerCase().includes(query) || selected.description?.toLowerCase().includes(query))) {
      matches.push(selected);
    }
    for (const option of options) {
      if (matches.length >= MAX_VISIBLE_OPTIONS) break;
      if (option.id === value) continue;
      if (!query || option.name.toLowerCase().includes(query) || option.description?.toLowerCase().includes(query)) {
        matches.push(option);
      }
    }
    return matches;
  }, [options, search, selected, value]);

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setSearch("");
      }}
    >
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="outline"
          role="combobox"
          id={triggerId}
          aria-expanded={open}
          disabled={disabled}
          data-testid={testId}
          style={{ fontWeight: 400 }}
          className={cn("w-full justify-between", !disabled && "cursor-pointer", className)}
        >
          <span className="truncate">{selected?.name || placeholder}</span>
          <IconChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="p-2" style={{ width: "min(22rem, calc(100vw - 2rem))" }}>
        <div className="space-y-2">
          <Input
            value={search}
            placeholder={t("commonSearch")}
            autoFocus
            onChange={(event) => setSearch(event.target.value)}
            className="w-full"
          />
          <div role="listbox" style={{ maxHeight: "16rem", overflowY: "auto" }}>
            {visible.length === 0 ? (
              <div className="px-2 py-3 text-xs text-muted-foreground">{t("noMatchingResults")}</div>
            ) : (
              visible.map((option) => {
                const isSelected = option.id === value;
                return (
                  <button
                    key={option.id}
                    type="button"
                    role="option"
                    aria-selected={isSelected}
                    onClick={() => {
                      const picked = pickedValue(value, option.id);
                      if (picked !== value) onChange(picked);
                      setOpen(false);
                      setSearch("");
                    }}
                    className="flex w-full cursor-pointer items-center justify-between gap-2 rounded-md px-2 py-1.5 text-left text-sm hover:bg-muted"
                  >
                    <span className="flex min-w-0 flex-col">
                      <span className="truncate">{option.name}</span>
                      {option.description && (
                        <span className="truncate text-xs text-muted-foreground">{option.description}</span>
                      )}
                    </span>
                    {isSelected && <IconCheck className="h-4 w-4 shrink-0" />}
                  </button>
                );
              })
            )}
          </div>
        </div>
      </PopoverContent>
    </Popover>
  );
}

/**
 * Kandev's searchable picker, for Azure DevOps lists that can hold hundreds or
 * thousands of projects, teams, boards, or repositories.
 */
export function SearchablePicker(props: {
  value: string;
  options: PickerOption[];
  onChange: (value: string) => void;
  placeholder: string;
  testId?: string;
  triggerId?: string;
  className?: string;
  disabled?: boolean;
}) {
  const { value, options, onChange, placeholder, testId, triggerId, className, disabled } = props;
  const t = useT();
  const items = useMemo(
    () =>
      options.length > LARGE_PICKER_THRESHOLD
        ? []
        : options.map((option) => ({
            value: option.id,
            label: option.name,
            ...(option.description
              ? {
                  description: option.description,
                  renderLabel: () => (
                    <span className="flex min-w-0 flex-col">
                      <span className="truncate">{option.name}</span>
                      <span className="truncate text-xs text-muted-foreground">{option.description}</span>
                    </span>
                  ),
                  renderTriggerLabel: () => <span className="truncate">{option.name}</span>,
                }
              : {}),
          })),
    [options],
  );
  if (options.length > LARGE_PICKER_THRESHOLD) {
    return <LargeSearchablePicker {...props} />;
  }
  return (
    <Combobox
      options={items}
      value={value}
      onValueChange={(next: string) => {
        const picked = pickedValue(value, next);
        if (picked !== value) onChange(picked);
      }}
      placeholder={placeholder}
      searchPlaceholder={t("commonSearch")}
      emptyMessage={t("noMatchingResults")}
      testId={testId}
      triggerId={triggerId}
      triggerClassName={className}
      disabled={disabled}
      filter={pickerFilter}
      showSearch
    />
  );
}
