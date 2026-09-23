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

export function ModeIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path d="M6 7h12M6 12h8M6 17h4" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
      <path d="m15 5 3 2-3 2" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
      <path d="m11 15 3 2-3 2" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
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

export function UpIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path
        d="m6 14 6-6 6 6"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </SvgIcon>
  )
}

export function AlbumsIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <rect x="8" y="4.5" width="11.5" height="8.5" rx="1.5" stroke="currentColor" strokeWidth="1.8" />
      <rect x="4.5" y="10.5" width="11.5" height="9" rx="1.5" stroke="currentColor" strokeWidth="1.8" />
    </SvgIcon>
  )
}

export function SearchIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <circle cx="11" cy="11" r="6.5" stroke="currentColor" strokeWidth="1.8" />
      <path
        d="m16 16 4.5 4.5"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </SvgIcon>
  )
}

export function UploadIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <path d="M12 15V4.5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
      <path
        d="m7.5 8.5 4.5-4 4.5 4"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <path
        d="M4.5 15.5v2A2.5 2.5 0 0 0 7 20h10a2.5 2.5 0 0 0 2.5-2.5v-2"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </SvgIcon>
  )
}

export function MoreIcon({ title }: IconProps) {
  return (
    <SvgIcon title={title}>
      <circle cx="5" cy="12" r="1.7" fill="currentColor" stroke="none" />
      <circle cx="12" cy="12" r="1.7" fill="currentColor" stroke="none" />
      <circle cx="19" cy="12" r="1.7" fill="currentColor" stroke="none" />
    </SvgIcon>
  )
}
