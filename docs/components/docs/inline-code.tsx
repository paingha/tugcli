import type { ComponentProps } from 'react';

/**
 * Inline code that never breaks inside a hyphenated word such as `--proxy`
 * or `env=prod`, but still wraps at spaces. Code blocks pass straight
 * through: their children are highlighted lines, not a string.
 */
export function InlineCode({ children, ...props }: ComponentProps<'code'>) {
  if (typeof children !== 'string') return <code {...props}>{children}</code>;
  const parts = children.split(/(\s+)/);
  return (
    <code {...props}>
      {parts.map((part, i) =>
        part.includes('-') && part.length <= 32 ? (
          <span key={i} className="whitespace-nowrap">
            {part}
          </span>
        ) : (
          part
        ),
      )}
    </code>
  );
}
