import { icons } from 'lucide-react';

/** A Lucide icon by name, for use in MDX: <Icon name="Compass" />. */
export function Icon({ name, className }: { name: keyof typeof icons; className?: string }) {
  const Svg = icons[name];
  return Svg ? <Svg className={className} /> : null;
}
