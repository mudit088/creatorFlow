/**
 * The shared building blocks.
 *
 * These exist so that a button in the checkout and a button in the dashboard
 * are the same button. Every variant below is a decision made once: how a
 * surface is raised, how a control responds to a press, what a disabled state
 * looks like. Pages compose these rather than re-deciding.
 */

/** Joins class names, dropping anything falsy so callers can use `cond && "x"`. */
export function cx(...parts) {
  return parts.filter(Boolean).join(" ");
}

const buttonBase =
  "inline-flex items-center justify-center gap-2 rounded-control font-medium " +
  "transition-[transform,background-color,border-color,box-shadow,opacity] duration-200 ease-soft " +
  // The press is the whole interaction: a control that moves under the cursor
  // feels responsive in a way no colour change does.
  "active:scale-[0.98] " +
  "disabled:pointer-events-none disabled:opacity-50";

const buttonVariants = {
  primary:
    "bg-brand-600 text-white shadow-subtle hover:bg-brand-700 hover:shadow-raised",
  secondary:
    "border border-line bg-card text-ink " +
    "hover:border-line-strong hover:bg-canvas",
  ghost: "text-ink-soft hover:bg-canvas hover:text-ink",
  danger: "bg-danger text-white hover:opacity-90",
};

const buttonSizes = {
  sm: "h-9 px-3.5 text-sm",
  md: "h-11 px-5 text-sm",
  lg: "h-12 px-6 text-base",
};

export function Button({
  variant = "primary",
  size = "md",
  className,
  as: Component = "button",
  loading = false,
  children,
  ...props
}) {
  return (
    <Component
      className={cx(buttonBase, buttonVariants[variant], buttonSizes[size], className)}
      // A loading button stays disabled so a double click cannot submit twice —
      // the backend is idempotent, but the user should not have to rely on that.
      disabled={loading || props.disabled}
      {...props}
    >
      {loading ? <Spinner /> : null}
      {children}
    </Component>
  );
}

export function Spinner({ className }) {
  return (
    <svg
      className={cx("size-4 animate-spin", className)}
      viewBox="0 0 24 24"
      fill="none"
      // Decorative: the surrounding button already says what is happening, and
      // announcing the spinner too would make a screen reader say it twice.
      aria-hidden="true"
    >
      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="3" />
      <path
        className="opacity-90"
        fill="currentColor"
        d="M12 2a10 10 0 0 1 10 10h-3a7 7 0 0 0-7-7V2Z"
      />
    </svg>
  );
}

export function Card({ className, as: Component = "div", ...props }) {
  return (
    <Component
      className={cx(
        "rounded-card border border-line bg-card shadow-subtle",
        className,
      )}
      {...props}
    />
  );
}

/**
 * A card that responds to the cursor. The lift is small — 2px and a slightly
 * deeper shadow — because the point is to confirm the thing is interactive,
 * not to animate.
 */
export function InteractiveCard({ className, as: Component = "div", ...props }) {
  return (
    <Component
      className={cx(
        "group relative rounded-card border border-line bg-card",
        "shadow-subtle transition-[transform,box-shadow,border-color] duration-250 ease-soft",
        "hover:-translate-y-0.5 hover:border-line-strong hover:shadow-raised",
        "active:translate-y-0 active:shadow-subtle",
        className,
      )}
      {...props}
    />
  );
}

export function Input({ className, invalid = false, ...props }) {
  return (
    <input
      className={cx(
        "h-11 w-full rounded-control border bg-card px-3.5 text-sm",
        "text-ink placeholder:text-ink-muted",
        "transition-[border-color,box-shadow] duration-200 ease-soft",
        invalid
          ? "border-danger"
          : "border-line hover:border-line-strong",
        className,
      )}
      // Tells assistive technology the field is rejected, which the red border
      // alone does not — colour is not available to everyone.
      aria-invalid={invalid || undefined}
      {...props}
    />
  );
}

/**
 * Field pairs a label with its control and error text.
 *
 * The error is rendered in a live region so a screen reader announces it when
 * it appears, rather than only when the user happens to navigate back to it.
 */
export function Field({ label, htmlFor, error, hint, children }) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-sm font-medium text-ink">
        {label}
      </label>
      {children}
      {error ? (
        <p role="alert" className="text-xs text-danger">
          {error}
        </p>
      ) : hint ? (
        <p className="text-xs text-ink-muted">{hint}</p>
      ) : null}
    </div>
  );
}

export function Badge({ className, children }) {
  return (
    <span
      className={cx(
        "inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium",
        "bg-brand-50 text-brand-700 dark:bg-brand-900/40 dark:text-brand-200",
        className,
      )}
    >
      {children}
    </span>
  );
}

/**
 * A generated avatar.
 *
 * Deriving the hue from the name means every creator gets a distinct, stable
 * colour with no upload and no stored asset — and the page looks designed
 * rather than empty before anyone has set a picture.
 */
export function Avatar({ name, size = 72 }) {
  const initials = name
    .split(/\s+/)
    .slice(0, 2)
    .map((word) => word[0])
    .join("")
    .toUpperCase();

  // A cheap deterministic hash. Not security, just a stable number per name.
  let hash = 0;
  for (let i = 0; i < name.length; i += 1) {
    hash = (hash * 31 + name.charCodeAt(i)) % 360;
  }

  // Constrained to the brand's neighbourhood — indigo through violet to pink —
  // rather than the full colour wheel. An unconstrained hash is how one creator
  // gets a green avatar on a violet page: still deterministic, still distinct,
  // and visibly not part of the same design.
  const hue = 258 + (hash % 84);

  return (
    <div
      className="flex shrink-0 items-center justify-center rounded-full font-semibold text-white shadow-raised ring-2 ring-white/70 dark:ring-white/10"
      style={{
        width: size,
        height: size,
        fontSize: size * 0.36,
        // Two stops 28 degrees apart: enough for the gradient to read as depth,
        // little enough that it stays one colour rather than becoming a sunset.
        background: `linear-gradient(140deg, oklch(0.68 0.17 ${hue}), oklch(0.53 0.21 ${hue + 28}))`,
      }}
      aria-hidden="true"
    >
      {initials}
    </div>
  );
}

/**
 * Skeletons hold the shape of content that has not arrived.
 *
 * Laying out the real dimensions matters: a spinner that is replaced by content
 * shifts the page, while a skeleton of the right size means nothing moves when
 * the data lands.
 */
export function Skeleton({ className }) {
  return (
    <div
      className={cx(
        "animate-pulse rounded-control bg-line",
        className,
      )}
    />
  );
}
