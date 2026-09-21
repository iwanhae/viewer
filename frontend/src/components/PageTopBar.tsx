import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import './pagetopbar.css'

type PageTopBarProps = {
  title: string
  actions?: ReactNode
}

// PageTopBar is the quiet header shared by the management pages (Find Albums,
// Upload): a way back to the wall, the page title, and room for one or two
// cross-links. The wall and the viewer keep their own chrome instead.
export function PageTopBar({ title, actions }: PageTopBarProps) {
  return (
    <header className="page-topbar">
      <Link className="page-topbar-home" to="/" data-testid="page-topbar-home">
        Wall
      </Link>
      <h1 className="page-topbar-title">{title}</h1>
      {actions !== undefined && <div className="page-topbar-actions">{actions}</div>}
    </header>
  )
}
