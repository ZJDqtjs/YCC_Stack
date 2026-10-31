import { createContext, useContext } from 'react'

export const AppCtx = createContext<any>(null)
export const useApp = () => useContext(AppCtx)
