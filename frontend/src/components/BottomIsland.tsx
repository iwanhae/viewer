import { useEffect, useRef, useState, type ReactNode } from 'react'

type PopupRenderContext = {
  close: () => void
}

type BottomIslandButtonAction = {
  kind?: 'button'
  id: string
  icon: ReactNode
  ariaLabel: string
  tooltip?: string
  testId?: string
  disabled?: boolean
  className?: string
  onClick?: () => void
  renderPopup?: (context: PopupRenderContext) => ReactNode
}

type BottomIslandIndicator = {
  kind: 'indicator'
  id: string
  label: ReactNode
  testId?: string
  className?: string
  ariaLabel?: string
}

type BottomIslandAction = BottomIslandButtonAction | BottomIslandIndicator

type BottomIslandProps = {
  actions: BottomIslandAction[]
  className?: string
}

export function BottomIsland({ actions, className }: BottomIslandProps): ReactNode {
  const [openActionId, setOpenActionId] = useState<string | null>(null)
  const rootRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const onDocumentMouseDown = (event: MouseEvent) => {
      const root = rootRef.current
      if (!root) return
      if (event.target instanceof Node && root.contains(event.target)) return
      setOpenActionId(null)
    }

    const onDocumentKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpenActionId(null)
      }
    }

    document.addEventListener('mousedown', onDocumentMouseDown)
    document.addEventListener('keydown', onDocumentKeyDown)
    return () => {
      document.removeEventListener('mousedown', onDocumentMouseDown)
      document.removeEventListener('keydown', onDocumentKeyDown)
    }
  }, [])

  return (
    <div className={`bottom-island ${className ?? ''}`.trim()} ref={rootRef}>
      {actions.map((action) => {
        if (action.kind === 'indicator') {
          return (
            <div
              className={`bottom-island-indicator ${action.className ?? ''}`.trim()}
              data-testid={action.testId}
              aria-label={action.ariaLabel}
              key={action.id}
            >
              {action.label}
            </div>
          )
        }

        const hasPopup = action.renderPopup !== undefined
        const isOpen = openActionId === action.id
        const tooltipText = action.tooltip ?? action.ariaLabel

        return (
          <div className="bottom-island-item" key={action.id}>
            <button
              type="button"
              className={`bottom-island-action ${action.className ?? ''} ${isOpen ? 'is-open' : ''}`.trim()}
              data-testid={action.testId}
              disabled={action.disabled}
              aria-label={action.ariaLabel}
              aria-expanded={hasPopup ? isOpen : undefined}
              aria-haspopup={hasPopup ? 'dialog' : undefined}
              onClick={() => {
                if (hasPopup) {
                  setOpenActionId((current) => (current === action.id ? null : action.id))
                  return
                }
                setOpenActionId(null)
                action.onClick?.()
              }}
            >
              {action.icon}
            </button>
            <span className="bottom-island-tooltip">{tooltipText}</span>
            {hasPopup && isOpen && (
              <div className="bottom-island-popup" role="dialog">
                {action.renderPopup?.({
                  close: () => setOpenActionId(null),
                })}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
