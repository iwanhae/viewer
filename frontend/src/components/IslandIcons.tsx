import type { ReactNode } from 'react'

type IconProps = {
  title?: string
}

type SvgIconProps = {
  children: ReactNode
  title?: string
}

function SvgIcon({ children, title }: SvgIconProps) {
  return (
    <svg
      className="bottom-island-icon"
      viewBox="0 0 24 24"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden={title ? undefined : true}
      role={title ? 'img' : undefined}
    >
      {title ? <title>{title}</title> : null}
      {children}
    </svg>
  )
}

export function ColumnsIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <rect x="4" y="4" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="10" y="4" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="16" y="4" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="4" y="10" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="10" y="10" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="16" y="10" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="4" y="16" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="10" y="16" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
      <rect x="16" y="16" width="4" height="4" rx="1.1" stroke="currentColor" strokeWidth="1.8" />
    </SvgIcon>
  )
}

export function RefreshIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path
        d="M20 12a8 8 0 1 1-2.34-5.66"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
      <path
        d="M20 5v5h-5"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </SvgIcon>
  )
}

export function ShortcutIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <circle cx="6.5" cy="6.5" r="2.4" stroke="currentColor" strokeWidth="1.8" />
      <circle cx="17.5" cy="6.5" r="2.4" stroke="currentColor" strokeWidth="1.8" />
      <circle cx="12" cy="17.5" r="2.4" stroke="currentColor" strokeWidth="1.8" />
      <path
        d="M8.5 8.1 10.8 15M15.5 8.1 13.2 15M8.8 6.5h6.4"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </SvgIcon>
  )
}

export function BackToAlbumIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path
        d="M11 6 5 12l6 6M6 12h13"
        stroke="currentColor"
        strokeWidth="1.9"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </SvgIcon>
  )
}

export function PrevIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path
        d="m15 18-6-6 6-6"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </SvgIcon>
  )
}

export function NextIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path
        d="m9 18 6-6-6-6"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </SvgIcon>
  )
}
