// Tailwind v3.4.17 configuration for the console stylesheet
// (internal/ui/static/tailwind.css). Moved out of the pageHead partial's inline
// script when the Play CDN was replaced by a prebuilt file (GitHub #165;
// web-ui-design.md "Self-hosted Tailwind and the console CSP").
//
// After changing this file, a template's classes, or a Go file that emits
// classes: `make ui-css`, and commit the regenerated stylesheet. `make lint`
// and CI fail when it is stale (`make ui-css-check`).
module.exports = {
    // Every file the console's class names come from. The Play CDN read the
    // live DOM; a build reads only these. TestTailwindContent_CoversEveryClassEmitter
    // fails when a file emits class= literals and is in neither this list nor
    // its table of non-console emitters. Paths are relative to this file.
    content: {
        relative: true,
        files: [
            '../templates/*.html',
            '../*.go',
            '!../*_test.go',
            '../../api/setup_handlers.go',
        ],
    },
    // Class names assembled at render time, which no scan can see:
    // the sortHeader partial builds text-{{$align}} (left/right).
    safelist: ['text-left', 'text-right', 'text-center'],
    // Two themes live in the same page, switched at runtime by data-theme on
    // <html>. Token values are CSS custom properties defined in the pageHead
    // partial's inline <style> block (_partials.html), so one attribute flip
    // re-paints every surface. Both themes follow the EaseIT Labs Brand Manual
    // v1.2 (ui-design-refresh-design.md §11).
    // Conventions baked into the token system:
    //   * `dark-*` is the SURFACE tier scale (page, card, raised, border). The
    //     name is historical; it does NOT mean "dark colour".
    //   * `gray-*` is the INK tier: gray-100 = primary text, gray-600 =
    //     disabled; the hex value depends on the active theme.
    //   * Status colours (amber, rose, emerald, violet, purple) stay Tailwind
    //     defaults: semantic signals, not brand identity.
    // Wire `dark:` variants to our data-theme attribute so
    // class strings like `text-brand-700 dark:text-brand-300`
    // resolve based on the operator's theme choice (not the
    // OS preference, which Tailwind's default 'media' mode
    // would lock us into).
    darkMode: ['selector', '[data-theme="dark"]'],
    theme: {
        extend: {
            fontFamily: {
                sans: ['Manrope', 'system-ui', '-apple-system', 'sans-serif'],
                display: ['"Space Grotesk"', 'Manrope', 'system-ui', 'sans-serif'],
                mono: ['ui-monospace', '"SF Mono"', '"Cascadia Code"', 'Menlo', 'Consolas', 'monospace'],
            },
            colors: {
                // Surface tiers (page → card → raised → border).
                dark: {
                    900: 'rgb(var(--surface-900) / <alpha-value>)',
                    800: 'rgb(var(--surface-800) / <alpha-value>)',
                    700: 'rgb(var(--surface-700) / <alpha-value>)',
                    600: 'rgb(var(--surface-600) / <alpha-value>)'
                },
                // Ink tier (semantically: 100 = primary text,
                // 600 = disabled). Tailwind's `gray` is the
                // most-used class family in the templates, so
                // overriding it lets every existing text-gray-*
                // use participate in theming without a sed.
                gray: {
                    100: 'rgb(var(--ink-100) / <alpha-value>)',
                    200: 'rgb(var(--ink-200) / <alpha-value>)',
                    300: 'rgb(var(--ink-300) / <alpha-value>)',
                    400: 'rgb(var(--ink-400) / <alpha-value>)',
                    500: 'rgb(var(--ink-500) / <alpha-value>)',
                    600: 'rgb(var(--ink-600) / <alpha-value>)',
                    700: 'rgb(var(--ink-700) / <alpha-value>)',
                    800: 'rgb(var(--ink-800) / <alpha-value>)',
                    900: 'rgb(var(--ink-900) / <alpha-value>)'
                },
                // Explicit semantic aliases (ui-per-page-polish).
                // `surface` == the --surface-* tier (SAME vars as
                // `dark` above); `ink` == the --ink-* tier (SAME
                // vars as `gray`). Registered so the clearer
                // `bg-surface-*` / `text-ink-*` class names actually
                // resolve under the Tailwind CDN JIT. The four
                // re-tiered pages (dashboard/task_detail/
                // project_detail/execution) use these names; the rest
                // of the templates still use dark/gray (identical
                // vars). MUST stay in sync with the dark/gray blocks.
                surface: {
                    900: 'rgb(var(--surface-900) / <alpha-value>)',
                    800: 'rgb(var(--surface-800) / <alpha-value>)',
                    700: 'rgb(var(--surface-700) / <alpha-value>)',
                    600: 'rgb(var(--surface-600) / <alpha-value>)'
                },
                ink: {
                    100: 'rgb(var(--ink-100) / <alpha-value>)',
                    200: 'rgb(var(--ink-200) / <alpha-value>)',
                    300: 'rgb(var(--ink-300) / <alpha-value>)',
                    400: 'rgb(var(--ink-400) / <alpha-value>)',
                    500: 'rgb(var(--ink-500) / <alpha-value>)',
                    600: 'rgb(var(--ink-600) / <alpha-value>)',
                    700: 'rgb(var(--ink-700) / <alpha-value>)',
                    800: 'rgb(var(--ink-800) / <alpha-value>)',
                    900: 'rgb(var(--ink-900) / <alpha-value>)'
                },
                // Form-control boundary (WCAG 1.4.11, >= 3:1 on every
                // surface; ui-design-refresh-design.md §11.3). Use
                // `border border-control` on inputs/selects/textareas
                // and toggle groups, never a surface-* border.
                control: 'rgb(var(--control-border) / <alpha-value>)',
                // Primary brand — varies per theme.
                brand: {
                    50:  'rgb(var(--brand-50) / <alpha-value>)',
                    100: 'rgb(var(--brand-100) / <alpha-value>)',
                    200: 'rgb(var(--brand-200) / <alpha-value>)',
                    300: 'rgb(var(--brand-300) / <alpha-value>)',
                    400: 'rgb(var(--brand-400) / <alpha-value>)',
                    500: 'rgb(var(--brand-500) / <alpha-value>)',
                    600: 'rgb(var(--brand-600) / <alpha-value>)',
                    700: 'rgb(var(--brand-700) / <alpha-value>)',
                    800: 'rgb(var(--brand-800) / <alpha-value>)',
                    900: 'rgb(var(--brand-900) / <alpha-value>)'
                },
                // Secondary accent — varies per theme.
                accent: {
                    50:  'rgb(var(--accent-50) / <alpha-value>)',
                    100: 'rgb(var(--accent-100) / <alpha-value>)',
                    200: 'rgb(var(--accent-200) / <alpha-value>)',
                    300: 'rgb(var(--accent-300) / <alpha-value>)',
                    400: 'rgb(var(--accent-400) / <alpha-value>)',
                    500: 'rgb(var(--accent-500) / <alpha-value>)',
                    600: 'rgb(var(--accent-600) / <alpha-value>)',
                    700: 'rgb(var(--accent-700) / <alpha-value>)',
                    800: 'rgb(var(--accent-800) / <alpha-value>)',
                    900: 'rgb(var(--accent-900) / <alpha-value>)'
                }
            }
        }
    }
};
